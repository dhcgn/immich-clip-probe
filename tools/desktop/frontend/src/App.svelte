<script lang="ts">
  import { onMount } from 'svelte';
  import { Events } from '@wailsio/runtime';
  import { CheckService, type ServerInfo } from '../bindings/github.com/dhcgn/immich-clip-probe/tools/desktop';
  import { Status, type Config, type Match, type Result } from '../bindings/github.com/dhcgn/immich-clip-probe/tools/check';

  let info: ServerInfo | null = $state(null);
  let cfg: Config | null = $state(null);
  let editing = $state(false);
  let results: Result[] = $state([]);
  let total = $state(0);
  let running = $state(false);
  let message = $state('');

  const label: Record<string, string> = {
    [Status.StatusMatch]: 'match',
    [Status.StatusNoMatch]: 'no match',
    [Status.StatusSkipped]: 'skipped',
    [Status.StatusError]: 'error',
  };
  let counts = $derived(
    results.reduce((c, r) => ((c[r.status] = (c[r.status] ?? 0) + 1), c), {} as Record<string, number>),
  );

  async function refresh() {
    info = await CheckService.Info();
    cfg = await CheckService.Settings();
    if (info.error === 'no server configured') editing = true;
  }

  async function start(paths: string[] | null) {
    if (!paths?.length || running) return;
    message = '';
    try {
      total = await CheckService.Check(paths);
      results = [];
      running = total > 0;
      if (total === 0) message = 'No images found there.';
    } catch (e) {
      message = String(e);
    }
  }

  async function pick(folder: boolean) {
    try {
      await start(await CheckService.Pick(folder));
    } catch (e) {
      message = String(e);
    }
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!cfg) return;
    try {
      await CheckService.SaveSettings(cfg);
      editing = false;
      await refresh();
    } catch (err) {
      message = String(err);
    }
  }

  function nearest(r: Result): Match | null {
    return r.matches?.[0] ?? null;
  }

  function name(path: string) {
    return path.split(/[\\/]/).pop();
  }

  onMount(() => {
    refresh();
    Events.On('result', (e) => (results = [...results, e.data]));
    Events.On('done', (e) => {
      running = false;
      if (e.data) message = e.data;
    });
    Events.On('dropped', (e) => start(e.data));
  });
</script>

<main data-file-drop-target>
  <header>
    <h1>Clip Check</h1>
    <div class="status">
      {#if !info}
        connecting…
      {:else if info.health}
        <span class:bad={info.health.status !== 'ok'}>
          {info.health.status === 'ok' ? '●' : '▲'} {info.health.model} · {info.health.indexedAssets.toLocaleString()} assets indexed
        </span>
        {#if info.health.message}<span class="bad">{info.health.message}</span>{/if}
      {:else}
        <span class="bad">▲ {info.error}</span>
      {/if}
      {#if info}
        <span class:bad={!info.magick} title={info.magick || 'HEIC, RAW, AVIF and JXL need ImageMagick'}>
          ImageMagick {info.magick ? 'found' : 'not found'}
        </span>
      {/if}
    </div>
    <button class="link" onclick={() => (editing = !editing)}>Settings</button>
  </header>

  {#if editing && cfg}
    <form class="settings" onsubmit={save}>
      <label>clip-probe server <input bind:value={cfg.server} placeholder="https://clip-probe.example.com" required /></label>
      <label>API token <input type="password" bind:value={cfg.token} required /></label>
      <label>Match up to distance <input type="number" step="0.001" min="0" max="2" bind:value={cfg.maxDistance} /></label>
      <label>Matches per file <input type="number" min="1" max="100" bind:value={cfg.limit} /></label>
      <label>Files in parallel <input type="number" min="1" max="32" bind:value={cfg.workers} /></label>
      <label>ImageMagick <input bind:value={cfg.magick} placeholder="find automatically, or off" /></label>
      <div class="row">
        <button type="submit">Save</button>
        <span class="muted">{info?.configPath}</span>
      </div>
    </form>
  {/if}

  <section class="actions">
    <button onclick={() => pick(false)} disabled={running}>Add files</button>
    <button onclick={() => pick(true)} disabled={running}>Add folder</button>
    {#if running}
      <progress value={results.length} max={total}></progress>
      <span>{results.length} / {total}</span>
      <button class="link" onclick={() => CheckService.Cancel()}>Cancel</button>
    {:else}
      <span class="muted">or drop images and folders anywhere in this window</span>
    {/if}
  </section>

  {#if message}<p class="message">{message}</p>{/if}

  {#if results.length}
    <p class="summary">
      {#each Object.entries(label) as [status, text]}
        <span class={counts[status] ? status : 'muted'}>{counts[status] ?? 0} {text}</span>
      {/each}
    </p>
    <table>
      <thead>
        <tr><th></th><th>Distance</th><th>File</th><th>Nearest in Immich</th><th></th></tr>
      </thead>
      <tbody>
        {#each results as r (r.path)}
          {@const m = nearest(r)}
          <tr>
            <td><span class="badge {r.status}">{label[r.status]}</span></td>
            <td class="num">{r.distance != null ? r.distance.toFixed(4) : ''}</td>
            <td title={r.path}>{name(r.path)}</td>
            <td>
              {#if m}
                {m.originalFileName || m.assetId}
                {#if m.localDateTime}<span class="muted">{m.localDateTime.slice(0, 10)}</span>{/if}
              {:else}
                <span class="muted">{r.error ?? ''}</span>
              {/if}
            </td>
            <td>
              {#if m?.links?.web}
                <button class="link" onclick={() => CheckService.OpenURL(m.links!.web)}>Open in Immich</button>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</main>

<style>
  main {
    min-height: 100vh;
    padding: 1rem 1.5rem 2rem;
    box-sizing: border-box;
  }
  :global(.file-drop-target-active) {
    outline: 3px dashed var(--accent);
    outline-offset: -8px;
  }
  header {
    display: flex;
    align-items: baseline;
    gap: 1.5rem;
  }
  h1 {
    font-size: 1.25rem;
    margin: 0;
  }
  .status {
    flex: 1;
    display: flex;
    gap: 1.25rem;
    flex-wrap: wrap;
    color: var(--muted);
  }
  .bad,
  .error {
    color: var(--bad);
  }
  .muted {
    color: var(--muted);
  }
  .settings {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr));
    gap: 0.75rem 1.5rem;
    margin: 1rem 0;
    padding: 1rem;
    border: 1px solid var(--line);
    border-radius: 8px;
  }
  .settings label {
    display: grid;
    gap: 0.25rem;
    font-size: 0.85rem;
    color: var(--muted);
  }
  .settings .row {
    grid-column: 1 / -1;
    display: flex;
    gap: 1rem;
    align-items: center;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin: 1.25rem 0 0.75rem;
  }
  progress {
    width: 12rem;
  }
  .message {
    color: var(--bad);
  }
  .summary {
    display: flex;
    gap: 1.25rem;
  }
  .match {
    color: var(--good);
  }
  table {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    font-weight: 500;
    color: var(--muted);
    border-bottom: 1px solid var(--line);
  }
  td,
  th {
    padding: 0.35rem 0.6rem;
  }
  tr:nth-child(even) td {
    background: var(--stripe);
  }
  .num {
    font-variant-numeric: tabular-nums;
  }
  .badge {
    display: inline-block;
    min-width: 5.5rem;
    padding: 0.1rem 0.5rem;
    border-radius: 999px;
    font-size: 0.8rem;
    text-align: center;
    background: var(--stripe);
  }
  .badge.match {
    background: var(--good);
    color: #fff;
  }
  .badge.error {
    background: var(--bad);
    color: #fff;
  }
</style>
