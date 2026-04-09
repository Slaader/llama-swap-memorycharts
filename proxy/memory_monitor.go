package proxy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ModelMemoryUsage struct {
	Model        string `json:"model"`
	PID          int    `json:"pid"`
	RuntimeBytes uint64 `json:"runtime_bytes"`
	KVBytes      uint64 `json:"kv_bytes"`
}

type MemorySnapshot struct {
	Timestamp             time.Time          `json:"timestamp"`
	TotalBytes            uint64             `json:"total_bytes"`
	FreeBytes             uint64             `json:"free_bytes"`
	ReclaimableBytes      uint64             `json:"reclaimable_bytes"`
	LlamaRuntimeBytes     uint64             `json:"llama_runtime_bytes"`
	LlamaKVBytes          uint64             `json:"llama_kv_bytes"`
	AppsBytes             uint64             `json:"apps_bytes"`
	SystemServicesBytes   uint64             `json:"system_services_bytes"`
	LlamaRuntimeByModel   []ModelMemoryUsage `json:"llama_runtime_by_model"`
	SupportedOnHost       bool               `json:"supported_on_host"`
	HostCollectionMessage string             `json:"host_collection_message,omitempty"`
}

type MemoryTimelinePoint struct {
	BucketStart         time.Time          `json:"bucket_start"`
	BucketEnd           time.Time          `json:"bucket_end"`
	SampleCount         uint64             `json:"sample_count"`
	FreeBytes           uint64             `json:"free_bytes"`
	ReclaimableBytes    uint64             `json:"reclaimable_bytes"`
	LlamaRuntimeBytes   uint64             `json:"llama_runtime_bytes"`
	LlamaKVBytes        uint64             `json:"llama_kv_bytes"`
	AppsBytes           uint64             `json:"apps_bytes"`
	SystemServicesBytes uint64             `json:"system_services_bytes"`
	LlamaRuntimeByModel []ModelMemoryUsage `json:"llama_runtime_by_model"`
	SupportedOnHost     bool               `json:"supported_on_host"`
	CollectionMessage   string             `json:"collection_message,omitempty"`
}

type memoryTimelineAccumulator struct {
	sampleCount         uint64
	freeBytesSum        uint64
	reclaimableBytesSum uint64
	llamaRuntimeBytes   uint64
	llamaKVBytes        uint64
	appsBytes           uint64
	systemServicesBytes uint64
	modelRuntimeSum     map[string]uint64
	modelKVSum          map[string]uint64
}

type memoryMonitor struct {
	pm             *ProxyManager
	bucketDuration time.Duration
	retention      time.Duration
	sampleInterval time.Duration

	mu       sync.RWMutex
	current  MemorySnapshot
	timeline map[int64]*memoryTimelineAccumulator
}

func newMemoryMonitor(pm *ProxyManager) *memoryMonitor {
	return &memoryMonitor{
		pm:             pm,
		bucketDuration: 2 * time.Hour,
		retention:      30 * 24 * time.Hour,
		sampleInterval: 1 * time.Minute,
		timeline:       make(map[int64]*memoryTimelineAccumulator),
		current: MemorySnapshot{
			Timestamp:       time.Now().UTC(),
			SupportedOnHost: false,
		},
	}
}

func (m *memoryMonitor) start(stop <-chan struct{}) {
	go func() {
		m.sample()
		t := time.NewTicker(m.sampleInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.sample()
			}
		}
	}()
}

func (m *memoryMonitor) getCurrent() MemorySnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *memoryMonitor) getTimeline(hours int, bucketHours int) []MemoryTimelinePoint {
	if hours <= 0 {
		hours = 24 * 30
	}
	if bucketHours <= 0 {
		bucketHours = 2
	}

	requestedBucketDuration := time.Duration(bucketHours) * time.Hour
	if requestedBucketDuration < m.bucketDuration {
		requestedBucketDuration = m.bucketDuration
		bucketHours = int(m.bucketDuration / time.Hour)
	}

	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	m.mu.RLock()
	defer m.mu.RUnlock()

	merged := make(map[int64]*memoryTimelineAccumulator)
	for key := range m.timeline {
		pointTime := time.Unix(key, 0).UTC()
		if pointTime.Before(cutoff) {
			continue
		}
		targetBucketStart := pointTime.Truncate(requestedBucketDuration).Unix()
		src := m.timeline[key]
		dst, ok := merged[targetBucketStart]
		if !ok {
			dst = &memoryTimelineAccumulator{
				modelRuntimeSum: make(map[string]uint64),
				modelKVSum:      make(map[string]uint64),
			}
			merged[targetBucketStart] = dst
		}

		dst.sampleCount += src.sampleCount
		dst.freeBytesSum += src.freeBytesSum
		dst.reclaimableBytesSum += src.reclaimableBytesSum
		dst.llamaRuntimeBytes += src.llamaRuntimeBytes
		dst.llamaKVBytes += src.llamaKVBytes
		dst.appsBytes += src.appsBytes
		dst.systemServicesBytes += src.systemServicesBytes
		for model, value := range src.modelRuntimeSum {
			dst.modelRuntimeSum[model] += value
		}
		for model, value := range src.modelKVSum {
			dst.modelKVSum[model] += value
		}
	}

	keys := make([]int64, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	points := make([]MemoryTimelinePoint, 0, len(keys))
	for _, key := range keys {
		acc := merged[key]
		if acc == nil || acc.sampleCount == 0 {
			continue
		}

		models := make([]ModelMemoryUsage, 0, len(acc.modelRuntimeSum))
		for model, runtimeSum := range acc.modelRuntimeSum {
			models = append(models, ModelMemoryUsage{
				Model:        model,
				RuntimeBytes: runtimeSum / acc.sampleCount,
				KVBytes:      acc.modelKVSum[model] / acc.sampleCount,
			})
		}
		sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })

		bucketStart := time.Unix(key, 0).UTC()
		points = append(points, MemoryTimelinePoint{
			BucketStart:         bucketStart,
			BucketEnd:           bucketStart.Add(requestedBucketDuration),
			SampleCount:         acc.sampleCount,
			FreeBytes:           acc.freeBytesSum / acc.sampleCount,
			ReclaimableBytes:    acc.reclaimableBytesSum / acc.sampleCount,
			LlamaRuntimeBytes:   acc.llamaRuntimeBytes / acc.sampleCount,
			LlamaKVBytes:        acc.llamaKVBytes / acc.sampleCount,
			AppsBytes:           acc.appsBytes / acc.sampleCount,
			SystemServicesBytes: acc.systemServicesBytes / acc.sampleCount,
			LlamaRuntimeByModel: models,
			SupportedOnHost:     true,
		})
	}

	return points
}

func (m *memoryMonitor) sample() {
	now := time.Now().UTC()
	snapshot, err := collectMemorySnapshot(m.pm, now)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		snapshot = m.current
		snapshot.Timestamp = now
		snapshot.SupportedOnHost = false
		snapshot.HostCollectionMessage = err.Error()
	}

	m.current = snapshot
	m.addToTimelineLocked(snapshot)
	m.gcLocked(now)
}

func (m *memoryMonitor) addToTimelineLocked(snapshot MemorySnapshot) {
	bucketStart := snapshot.Timestamp.Truncate(m.bucketDuration).Unix()
	acc, ok := m.timeline[bucketStart]
	if !ok {
		acc = &memoryTimelineAccumulator{
			modelRuntimeSum: make(map[string]uint64),
			modelKVSum:      make(map[string]uint64),
		}
		m.timeline[bucketStart] = acc
	}

	acc.sampleCount++
	acc.freeBytesSum += snapshot.FreeBytes
	acc.reclaimableBytesSum += snapshot.ReclaimableBytes
	acc.llamaRuntimeBytes += snapshot.LlamaRuntimeBytes
	acc.llamaKVBytes += snapshot.LlamaKVBytes
	acc.appsBytes += snapshot.AppsBytes
	acc.systemServicesBytes += snapshot.SystemServicesBytes

	for _, model := range snapshot.LlamaRuntimeByModel {
		acc.modelRuntimeSum[model.Model] += model.RuntimeBytes
		acc.modelKVSum[model.Model] += model.KVBytes
	}
}

func (m *memoryMonitor) gcLocked(now time.Time) {
	cutoff := now.Add(-m.retention).Unix()
	for key := range m.timeline {
		if key < cutoff {
			delete(m.timeline, key)
		}
	}
}

type runningModelProcess struct {
	model  string
	pid    int
	logger *LogMonitor
}

type processSnapshot struct {
	pid int
	uid int
	rss uint64 // bytes
	cmd string
}

func collectMemorySnapshot(pm *ProxyManager, now time.Time) (MemorySnapshot, error) {
	total, free, reclaimable, err := collectSystemMemory()
	if err != nil {
		return MemorySnapshot{}, err
	}

	processes, err := collectProcessSnapshots()
	if err != nil {
		return MemorySnapshot{}, err
	}

	modelProcs := pm.getRunningModelProcesses()
	modelPIDSet := make(map[int]struct{}, len(modelProcs))
	processByPID := make(map[int]processSnapshot, len(processes))
	for _, proc := range processes {
		processByPID[proc.pid] = proc
	}

	modelRows := make([]ModelMemoryUsage, 0, len(modelProcs))
	var llamaRuntimeTotal uint64
	var llamaKVTotal uint64
	for _, modelProc := range modelProcs {
		modelPIDSet[modelProc.pid] = struct{}{}
		runtimeBytes := uint64(0)
		if proc, ok := processByPID[modelProc.pid]; ok {
			runtimeBytes = proc.rss
		}
		kvBytes := parseKVBytes(modelProc.logger.GetHistory())
		llamaRuntimeTotal += runtimeBytes
		llamaKVTotal += kvBytes
		modelRows = append(modelRows, ModelMemoryUsage{
			Model:        modelProc.model,
			PID:          modelProc.pid,
			RuntimeBytes: runtimeBytes,
			KVBytes:      kvBytes,
		})
	}
	sort.Slice(modelRows, func(i, j int) bool { return modelRows[i].Model < modelRows[j].Model })

	currentUID := os.Getuid()
	var appsBytes uint64
	for _, proc := range processes {
		if proc.uid != currentUID {
			continue
		}
		if _, isModelProc := modelPIDSet[proc.pid]; isModelProc {
			continue
		}
		appsBytes += proc.rss
	}

	usedBytes := uint64(0)
	if total > free {
		usedBytes = total - free
	}

	systemServices := usedBytes
	if systemServices > reclaimable {
		systemServices -= reclaimable
	} else {
		systemServices = 0
	}
	if systemServices > llamaRuntimeTotal {
		systemServices -= llamaRuntimeTotal
	} else {
		systemServices = 0
	}
	if systemServices > appsBytes {
		systemServices -= appsBytes
	} else {
		systemServices = 0
	}

	return MemorySnapshot{
		Timestamp:           now,
		TotalBytes:          total,
		FreeBytes:           free,
		ReclaimableBytes:    reclaimable,
		LlamaRuntimeBytes:   llamaRuntimeTotal,
		LlamaKVBytes:        llamaKVTotal,
		AppsBytes:           appsBytes,
		SystemServicesBytes: systemServices,
		LlamaRuntimeByModel: modelRows,
		SupportedOnHost:     true,
	}, nil
}

func collectSystemMemory() (total uint64, free uint64, reclaimable uint64, err error) {
	total, err = readMemTotal()
	if err != nil {
		return 0, 0, 0, err
	}

	pageSize, fields, err := readVMStat()
	if err != nil {
		return 0, 0, 0, err
	}

	freePages := fields["Pages free"] + fields["Pages speculative"]
	reclaimablePages := fields["Pages inactive"]

	free = freePages * uint64(pageSize)
	reclaimable = reclaimablePages * uint64(pageSize)
	return total, free, reclaimable, nil
}

func readMemTotal() (uint64, error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, fmt.Errorf("read hw.memsize: %w", err)
	}
	value := strings.TrimSpace(string(out))
	num, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hw.memsize %q: %w", value, err)
	}
	return num, nil
}

func readVMStat() (pageSize int, fields map[string]uint64, err error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, nil, fmt.Errorf("run vm_stat: %w", err)
	}

	pageSize = 16384
	pageSizeRE := regexp.MustCompile(`page size of ([0-9]+) bytes`)
	m := pageSizeRE.FindStringSubmatch(string(out))
	if len(m) == 2 {
		if parsed, parseErr := strconv.Atoi(m[1]); parseErr == nil && parsed > 0 {
			pageSize = parsed
		}
	}

	fields = make(map[string]uint64)
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valueRaw := strings.TrimSpace(parts[1])
		valueRaw = strings.TrimSuffix(valueRaw, ".")
		valueRaw = strings.ReplaceAll(valueRaw, ".", "")
		if valueRaw == "" {
			continue
		}
		value, parseErr := strconv.ParseUint(valueRaw, 10, 64)
		if parseErr != nil {
			continue
		}
		fields[key] = value
	}

	return pageSize, fields, nil
}

func collectProcessSnapshots() ([]processSnapshot, error) {
	out, err := exec.Command("ps", "-axo", "pid,uid,rss,comm").Output()
	if err != nil {
		return nil, fmt.Errorf("run ps: %w", err)
	}

	lines := strings.Split(string(out), "\n")
	rows := make([]processSnapshot, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PID") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		uid, uidErr := strconv.Atoi(fields[1])
		rssKB, rssErr := strconv.ParseUint(fields[2], 10, 64)
		if pidErr != nil || uidErr != nil || rssErr != nil {
			continue
		}
		cmd := strings.Join(fields[3:], " ")
		rows = append(rows, processSnapshot{
			pid: pid,
			uid: uid,
			rss: rssKB * 1024,
			cmd: cmd,
		})
	}
	return rows, nil
}

var kvLineRE = regexp.MustCompile(`KV buffer size =\s*([0-9]+(?:\.[0-9]+)?)\s*MiB`)

func parseKVBytes(history []byte) uint64 {
	if len(history) == 0 {
		return 0
	}

	lines := bytes.Split(history, []byte{'\n'})
	var totalMiB float64
	foundAny := false

	for i := len(lines) - 1; i >= 0; i-- {
		line := string(lines[i])
		if strings.Contains(line, "main: loading model") && foundAny {
			break
		}
		m := kvLineRE.FindStringSubmatch(line)
		if len(m) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		foundAny = true
		totalMiB += value
	}

	return uint64(totalMiB * 1024 * 1024)
}

func (pm *ProxyManager) getRunningModelProcesses() []runningModelProcess {
	pm.Lock()
	defer pm.Unlock()

	rows := make([]runningModelProcess, 0, len(pm.processGroups))
	for _, processGroup := range pm.processGroups {
		for _, process := range processGroup.processes {
			if process.CurrentState() != StateReady {
				continue
			}
			pid := process.PID()
			if pid <= 0 {
				continue
			}
			rows = append(rows, runningModelProcess{
				model:  process.ID,
				pid:    pid,
				logger: process.Logger(),
			})
		}
	}
	return rows
}
