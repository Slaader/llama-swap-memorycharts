<script lang="ts">
  import { onMount } from "svelte";
  import { memoryCurrent, memoryTimeline, fetchMemoryCurrent, fetchMemoryTimeline } from "../stores/api";
  import type { MemoryTimelinePoint, ModelMemoryUsage } from "../lib/types";

  const GIB = 1024 * 1024 * 1024;

  type PeriodKey = "month" | "week" | "day";
  type SeriesKey = "free" | "reclaimable" | "llama_runtime" | "llama_kv" | "apps" | "system_services";

  const periodOptions: Record<PeriodKey, { label: string; hours: number; bucketHours: number }> = {
    month: { label: "Month (weekly)", hours: 24 * 30, bucketHours: 24 * 7 },
    week: { label: "Week (daily)", hours: 24 * 7, bucketHours: 24 },
    day: { label: "Day (2h)", hours: 24, bucketHours: 2 },
  };

  const seriesConfig: Array<{ key: SeriesKey; label: string; color: string }> = [
    { key: "free", label: "Free", color: "#10b981" },
    { key: "reclaimable", label: "Reclaimable", color: "#06b6d4" },
    { key: "llama_runtime", label: "Llama Runtime", color: "#8b5cf6" },
    { key: "llama_kv", label: "Llama KV", color: "#d946ef" },
    { key: "apps", label: "Apps", color: "#f59e0b" },
    { key: "system_services", label: "System Services", color: "#64748b" },
  ];

  const seriesByKey: Record<SeriesKey, { label: string; color: string }> = {
    free: { label: "Free", color: "#10b981" },
    reclaimable: { label: "Reclaimable", color: "#06b6d4" },
    llama_runtime: { label: "Llama Runtime", color: "#8b5cf6" },
    llama_kv: { label: "Llama KV", color: "#d946ef" },
    apps: { label: "Apps", color: "#f59e0b" },
    system_services: { label: "System Services", color: "#64748b" },
  };

  let loading = $state(true);
  let error = $state("");
  let selectedPeriod = $state<PeriodKey>("month");
  let refreshSeconds = $state(15);
  let currentTimer: ReturnType<typeof setInterval> | null = null;
  let hoverIndex = $state(-1);

  let visibleSeries = $state<Record<SeriesKey, boolean>>({
    free: true,
    reclaimable: true,
    llama_runtime: true,
    llama_kv: true,
    apps: true,
    system_services: true,
  });

  const chartWidth = 980;
  const chartHeight = 280;
  const padX = 44;
  const padY = 16;

  const timelinePoints = $derived(
    [...$memoryTimeline].sort(
      (a, b) => new Date(a.bucket_start).getTime() - new Date(b.bucket_start).getTime(),
    ),
  );

  const activeSeries = $derived(seriesConfig.filter((s) => visibleSeries[s.key]));

  function valueFor(point: MemoryTimelinePoint, key: SeriesKey): number {
    if (key === "free") return point.free_bytes;
    if (key === "reclaimable") return point.reclaimable_bytes;
    if (key === "llama_runtime") return point.llama_runtime_bytes;
    if (key === "llama_kv") return point.llama_kv_bytes;
    if (key === "apps") return point.apps_bytes;
    return point.system_services_bytes;
  }

  function currentValueFor(key: SeriesKey): number {
    const current = $memoryCurrent;
    if (!current) return 0;
    if (key === "free") return current.free_bytes;
    if (key === "reclaimable") return current.reclaimable_bytes;
    if (key === "llama_runtime") return current.llama_runtime_bytes;
    if (key === "llama_kv") return current.llama_kv_bytes;
    if (key === "apps") return current.apps_bytes;
    return current.system_services_bytes;
  }

  const maxStackBytes = $derived(() => {
    let max = 0;
    for (const p of timelinePoints) {
      let total = 0;
      for (const s of activeSeries) {
        total += valueFor(p, s.key);
      }
      if (total > max) max = total;
    }
    return max || 1;
  });

  const hoverPoint = $derived(hoverIndex >= 0 ? timelinePoints[hoverIndex] : null);

  function formatGiB(bytes: number): string {
    return `${(bytes / GIB).toFixed(2)} GiB`;
  }

  function pct(bytes: number, total: number): number {
    if (!total || total <= 0) return 0;
    return Math.max(0, Math.min(100, (bytes / total) * 100));
  }

  function byRuntimeDesc(a: ModelMemoryUsage, b: ModelMemoryUsage): number {
    return b.runtime_bytes - a.runtime_bytes;
  }

  function xAt(i: number, count: number): number {
    if (count <= 1) return padX;
    return padX + (i * (chartWidth - 2 * padX)) / (count - 1);
  }

  function yAt(value: number): number {
    return chartHeight - padY - (value / maxStackBytes) * (chartHeight - 2 * padY);
  }

  function buildStackedAreaPath(top: number[], bottom: number[]): string {
    if (top.length === 0) return "";
    const count = top.length;
    const parts: string[] = [];
    parts.push(`M ${xAt(0, count)} ${yAt(top[0])}`);
    for (let i = 1; i < count; i++) {
      parts.push(`L ${xAt(i, count)} ${yAt(top[i])}`);
    }
    for (let i = count - 1; i >= 0; i--) {
      parts.push(`L ${xAt(i, count)} ${yAt(bottom[i])}`);
    }
    parts.push("Z");
    return parts.join(" ");
  }

  const stackedPaths = $derived(() => {
    const n = timelinePoints.length;
    if (n === 0) {
      return [] as Array<{ key: SeriesKey; path: string; color: string }>;
    }

    const cumulative = new Array(n).fill(0);
    const out: Array<{ key: SeriesKey; path: string; color: string }> = [];

    for (const series of activeSeries) {
      const bottom = [...cumulative];
      const top = new Array(n).fill(0);
      for (let i = 0; i < n; i++) {
        cumulative[i] += valueFor(timelinePoints[i], series.key);
        top[i] = cumulative[i];
      }
      out.push({ key: series.key, path: buildStackedAreaPath(top, bottom), color: series.color });
    }

    return out;
  });

  function toggleSeries(key: SeriesKey) {
    visibleSeries = { ...visibleSeries, [key]: !visibleSeries[key] };
  }

  function onChartMove(event: MouseEvent) {
    if (timelinePoints.length === 0) {
      hoverIndex = -1;
      return;
    }

    const svg = event.currentTarget as SVGRectElement;
    const box = svg.getBoundingClientRect();
    const x = event.clientX - box.left;
    const ratio = box.width > 0 ? Math.max(0, Math.min(1, x / box.width)) : 0;
    hoverIndex = Math.round(ratio * (timelinePoints.length - 1));
  }

  function onChartLeave() {
    hoverIndex = -1;
  }

  async function refreshCurrent() {
    await fetchMemoryCurrent();
  }

  async function refreshTimeline() {
    const settings = periodOptions[selectedPeriod];
    await fetchMemoryTimeline(settings.hours, settings.bucketHours);
  }

  async function refreshAll() {
    error = "";
    const current = await fetchMemoryCurrent();
    await refreshTimeline();
    if (!current) {
      error = "Unable to load memory telemetry from API.";
    }
    loading = false;
  }

  function resetCurrentTimer() {
    if (currentTimer) {
      clearInterval(currentTimer);
      currentTimer = null;
    }
    if (refreshSeconds > 0) {
      currentTimer = setInterval(() => {
        fetchMemoryCurrent();
      }, refreshSeconds * 1000);
    }
  }

  $effect(() => {
    selectedPeriod;
    if (!loading) {
      refreshTimeline();
    }
  });

  $effect(() => {
    refreshSeconds;
    resetCurrentTimer();
  });

  onMount(() => {
    refreshAll();
    resetCurrentTimer();
    return () => {
      if (currentTimer) clearInterval(currentTimer);
    };
  });
</script>

<div class="p-2 space-y-4">
  <h1 class="text-2xl font-bold">Memory</h1>

  {#if loading}
    <div class="text-center py-8">Loading memory telemetry...</div>
  {:else if error}
    <div class="card p-4 text-red-600 dark:text-red-400">{error}</div>
  {:else if !$memoryCurrent}
    <div class="card p-4">No memory snapshot available.</div>
  {:else}
    <section class="card p-4 space-y-3">
      <div class="flex items-center justify-between">
        <h2 class="text-lg font-semibold">Current</h2>
        <div class="flex items-center gap-2 text-sm">
          <label for="refresh-current">Auto refresh</label>
          <select id="refresh-current" bind:value={refreshSeconds} class="px-2 py-1 rounded border">
            <option value={5}>5s</option>
            <option value={15}>15s</option>
            <option value={30}>30s</option>
            <option value={60}>60s</option>
            <option value={0}>Off</option>
          </select>
          <button class="btn btn--sm" onclick={refreshCurrent}>Refresh</button>
        </div>
      </div>

      <div class="grid gap-2 md:grid-cols-2 lg:grid-cols-3 text-sm">
        <div>Total: {formatGiB($memoryCurrent.total_bytes)}</div>
        <div>Free: {formatGiB($memoryCurrent.free_bytes)}</div>
        <div>Reclaimable: {formatGiB($memoryCurrent.reclaimable_bytes)}</div>
        <div>Llama Runtime: {formatGiB($memoryCurrent.llama_runtime_bytes)}</div>
        <div>Llama KV: {formatGiB($memoryCurrent.llama_kv_bytes)}</div>
        <div>Apps: {formatGiB($memoryCurrent.apps_bytes)}</div>
        <div>System Services: {formatGiB($memoryCurrent.system_services_bytes)}</div>
      </div>

      <div class="space-y-1">
        <div class="text-xs text-gray-600 dark:text-gray-300">Current memory composition</div>
        <div class="h-4 w-full rounded overflow-hidden bg-gray-200 dark:bg-gray-700 flex">
          {#each seriesConfig as series}
            {#if visibleSeries[series.key]}
              <div class="h-full" style={`background:${series.color};width:${pct(currentValueFor(series.key), $memoryCurrent.total_bytes)}%`}></div>
            {/if}
          {/each}
        </div>
      </div>

      <div class="flex flex-wrap gap-2 text-xs">
        {#each seriesConfig as series}
          <button
            class={`px-2 py-1 rounded border ${visibleSeries[series.key] ? "opacity-100" : "opacity-40"}`}
            onclick={() => toggleSeries(series.key)}
            title="Toggle series"
          >
            <span class="inline-block w-3 h-3 mr-1 align-middle rounded" style={`background:${series.color}`}></span>{series.label}
          </button>
        {/each}
      </div>

      <h3 class="font-semibold pt-2">Per-model runtime and KV</h3>
      <div class="overflow-auto">
        <table class="min-w-full divide-y">
          <thead>
            <tr class="text-left text-xs uppercase tracking-wider">
              <th class="px-4 py-2">Model</th>
              <th class="px-4 py-2">PID</th>
              <th class="px-4 py-2">Runtime</th>
              <th class="px-4 py-2">KV</th>
            </tr>
          </thead>
          <tbody class="divide-y">
            {#each [...$memoryCurrent.llama_runtime_by_model].sort(byRuntimeDesc) as row}
              <tr class="text-sm">
                <td class="px-4 py-2">{row.model}</td>
                <td class="px-4 py-2">{row.pid || "-"}</td>
                <td class="px-4 py-2">{formatGiB(row.runtime_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(row.kv_bytes)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>

    <section class="card p-4 space-y-3">
      <div class="flex items-center justify-between">
        <h2 class="text-lg font-semibold">Timeline</h2>
        <div class="flex items-center gap-2 text-sm">
          <label for="timeline-range">Range</label>
          <select id="timeline-range" bind:value={selectedPeriod} class="px-2 py-1 rounded border">
            <option value="month">{periodOptions.month.label}</option>
            <option value="week">{periodOptions.week.label}</option>
            <option value="day">{periodOptions.day.label}</option>
          </select>
          <button class="btn btn--sm" onclick={refreshTimeline}>Refresh</button>
        </div>
      </div>

      <div class="flex flex-wrap gap-2 text-xs">
        {#each seriesConfig as series}
          <button
            class={`px-2 py-1 rounded border ${visibleSeries[series.key] ? "opacity-100" : "opacity-40"}`}
            onclick={() => toggleSeries(series.key)}
            title="Toggle series"
          >
            <span class="inline-block w-3 h-3 mr-1 align-middle rounded" style={`background:${series.color}`}></span>{series.label}
          </button>
        {/each}
      </div>

      <div class="overflow-auto border rounded p-2">
        {#if timelinePoints.length === 0}
          <div class="text-sm text-gray-600 dark:text-gray-300">No timeline points yet.</div>
        {:else}
          <svg viewBox={`0 0 ${chartWidth} ${chartHeight}`} class="w-full min-w-[900px] h-[320px]">
            <rect x="0" y="0" width={chartWidth} height={chartHeight} fill="transparent"></rect>
            <line x1={padX} y1={chartHeight - padY} x2={chartWidth - padX} y2={chartHeight - padY} stroke="#94a3b8" stroke-width="1"></line>
            <line x1={padX} y1={padY} x2={padX} y2={chartHeight - padY} stroke="#94a3b8" stroke-width="1"></line>

            {#each stackedPaths as series}
              <path d={series.path} fill={series.color} fill-opacity="0.5" stroke={series.color} stroke-width="1"></path>
            {/each}

            {#if hoverPoint && hoverIndex >= 0}
              <line
                x1={xAt(hoverIndex, timelinePoints.length)}
                y1={padY}
                x2={xAt(hoverIndex, timelinePoints.length)}
                y2={chartHeight - padY}
                stroke="#0f172a"
                stroke-dasharray="3 3"
                stroke-width="1"
              ></line>
            {/if}

            <text x={padX} y={chartHeight - 2} font-size="10" fill="#64748b">
              {new Date(timelinePoints[0].bucket_start).toLocaleString()}
            </text>
            <text x={chartWidth / 2 - 30} y={chartHeight - 2} font-size="10" fill="#64748b">
              {new Date(timelinePoints[Math.floor((timelinePoints.length - 1) / 2)].bucket_start).toLocaleString()}
            </text>
            <text x={chartWidth - 220} y={chartHeight - 2} font-size="10" fill="#64748b">
              {new Date(timelinePoints[timelinePoints.length - 1].bucket_start).toLocaleString()}
            </text>

            <rect
              x={padX}
              y={padY}
              width={chartWidth - 2 * padX}
              height={chartHeight - 2 * padY}
              fill="transparent"
              onmousemove={onChartMove}
              onmouseleave={onChartLeave}
              role="presentation"
              aria-hidden="true"
            ></rect>
          </svg>

          {#if hoverPoint}
            <div class="mt-2 text-xs rounded border p-2 bg-gray-50 dark:bg-gray-900/40">
              <div class="font-semibold">{new Date(hoverPoint.bucket_start).toLocaleString()}</div>
              <div class="mt-1 flex flex-wrap gap-3">
                {#each activeSeries as series}
                  <span>
                    <span class="inline-block w-2 h-2 mr-1 align-middle rounded" style={`background:${seriesByKey[series.key].color}`}></span>
                    {seriesByKey[series.key].label}: {formatGiB(valueFor(hoverPoint, series.key))}
                  </span>
                {/each}
              </div>
            </div>
          {/if}
        {/if}
      </div>

      <div class="overflow-auto">
        <table class="min-w-full divide-y">
          <thead>
            <tr class="text-left text-xs uppercase tracking-wider">
              <th class="px-4 py-2">Bucket Start</th>
              <th class="px-4 py-2">Samples</th>
              <th class="px-4 py-2">Free</th>
              <th class="px-4 py-2">Reclaimable</th>
              <th class="px-4 py-2">Llama Runtime</th>
              <th class="px-4 py-2">Llama KV</th>
              <th class="px-4 py-2">Apps</th>
              <th class="px-4 py-2">System Services</th>
            </tr>
          </thead>
          <tbody class="divide-y">
            {#each [...timelinePoints].reverse() as point}
              <tr class="text-sm">
                <td class="px-4 py-2">{new Date(point.bucket_start).toLocaleString()}</td>
                <td class="px-4 py-2">{point.sample_count}</td>
                <td class="px-4 py-2">{formatGiB(point.free_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(point.reclaimable_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(point.llama_runtime_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(point.llama_kv_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(point.apps_bytes)}</td>
                <td class="px-4 py-2">{formatGiB(point.system_services_bytes)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>
  {/if}
</div>
