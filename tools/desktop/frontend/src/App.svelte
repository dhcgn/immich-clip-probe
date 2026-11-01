<script lang="ts">
  import { onMount } from 'svelte';
  import { Events } from '@wailsio/runtime';
  import { CheckService, type ServerInfo } from '../bindings/github.com/dhcgn/immich-clip-probe/tools/desktop';
  import { Status, type Config, type Match, type Result } from '../bindings/github.com/dhcgn/immich-clip-probe/tools/check';

  let info: ServerInfo | null = $state(null);
  let cfg: Config | null = $state(null);
  let thumbs = $state(false); // Immich thumbnails, as saved rather than as being typed
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

  // A miss this close is usually an asset embedded before Immich 3.3.0, which
  // changed how embeddings are computed; see the README's Limitations.
  let nearMiss = $derived(results.some((r) => r.status === Status.StatusNoMatch && r.distance != null && r.distance < 0.06));

  // The status bar: green works, red is broken, grey is not set up (or still checking).
  type Light = 'ok' | 'bad' | 'off';
  let lights = $derived.by((): [string, Light, string][] => {
    if (!info) return [['clip-probe', 'off', 'checking…'], ['Immich', 'off', 'checking…']];
    const h = info.health;
    const probe: [string, Light, string] = h
      ? h.status === 'ok'
        ? ['clip-probe', 'ok', `${h.model} · ${h.indexedAssets.toLocaleString()} assets indexed`]
        : ['clip-probe', 'bad', h.message || `db ${h.db}, ml ${h.ml}`]
      : info.error === 'no server configured'
        ? ['clip-probe', 'off', 'not configured']
        : ['clip-probe', 'bad', info.error ?? ''];
    const im = info.immich;
    const immich: [string, Light, string] = !im
      ? ['Immich', 'off', 'not configured (optional, for thumbnails)']
      : im.error
        ? ['Immich', 'bad', im.error]
        : ['Immich', 'ok', im.owner || `key “${im.key}”`];
    const magick: [string, Light, string] = info.magick
      ? ['ImageMagick', 'ok', 'found']
      : ['ImageMagick', 'off', 'not found: HEIC, RAW, AVIF and JXL are skipped'];
    return [probe, immich, magick];
  });

  async function refresh() {
    info = null;
    info = await CheckService.Info();
    cfg = await CheckService.Settings();
    thumbs = !!(cfg.immichUrl && cfg.immichApiKey);
    if (info.error === 'no server configured') editing = true;
  }

  async function browseMagick() {
    const p = await CheckService.PickMagick();
    if (p && cfg) cfg.magick = p;
  }

  // A missing thumbnail keeps its space, so the rows stay aligned.
  function hide(e: Event) {
    (e.currentTarget as HTMLImageElement).style.visibility = 'hidden';
  }

  async function start(paths: string[] | null) {
    if (!paths?.length) return;
    if (running) {
      message = 'A check is still running; wait for it or cancel it first.';
      return;
    }
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
      {#each lights as [name, light, text]}
        <span class="svc" title={text}><i class="dot {light}"></i>{name} <span class="muted">{text}</span></span>
      {/each}
    </div>
    <button class="link" onclick={refresh} title="Check the connections again">Refresh</button>
    <button class="link" onclick={() => (editing = !editing)}>Settings</button>
  </header>

  {#if editing && cfg}
    <form class="settings" onsubmit={save}>
      <h2>clip-probe</h2>
      <label>Server URL <input bind:value={cfg.server} placeholder="https://clip-probe.example.com" required /></label>
      <label>API token <input type="password" bind:value={cfg.token} required /></label>
      <label>Match up to distance <input type="number" step="0.001" min="0" max="2" bind:value={cfg.maxDistance} /></label>
      <label>Matches per file <input type="number" min="1" max="100" bind:value={cfg.limit} /></label>
      <label>Files in parallel <input type="number" min="1" max="32" bind:value={cfg.workers} /></label>

      <h2>Immich thumbnails <span class="muted">optional</span></h2>
      <label>Immich URL <input bind:value={cfg.immichUrl} placeholder="https://immich.example.com" /></label>
      <label>Immich API key <input type="password" bind:value={cfg.immichApiKey} /></label>
      <p class="hint">With both set, every result also shows the thumbnail of the matching Immich asset. A key with only the <code>asset.view</code> permission is enough.</p>

      <h2>ImageMagick <span class="muted">optional</span></h2>
      <label class="wide">
        Path to the program (magick.exe)
        <span class="pick">
          <input bind:value={cfg.magick} placeholder={info?.magick ? `automatic: ${info.magick}` : 'automatic: not found'} />
          <button type="button" onclick={browseMagick}>Browse…</button>
        </span>
      </label>
      <p class="hint">Leave it empty to find ImageMagick automatically, or type <code>off</code> to never use it. With ImageMagick every image is rendered the way Immich renders it, which gives the closest distances, and HEIC, RAW, AVIF and JXL can be checked at all.</p>

      <div class="row">
        <button type="submit">Save</button>
        <span class="muted">saved to {info?.configPath}</span>
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
    {#if nearMiss}
      <p class="note">
        Close, but over the threshold? Assets added to Immich before an upgrade to <strong>Immich 3.3.0</strong>
        keep embeddings from Immich's old internal computation. Those sit about 0.02–0.05 from their own photo.
        Run <strong>Administration → Jobs → Smart Search → All</strong> once in Immich to re-embed them.
        The job first deletes every stored vector, so check again only after it has finished, when the indexed
        count in the status bar is back to where it was.
      </p>
    {/if}
    <table>
      <thead>
        <tr><th></th><th>Distance</th><th>Local file</th><th>Nearest in Immich</th><th></th></tr>
      </thead>
      <tbody>
        {#each results as r (r.path)}
          {@const m = nearest(r)}
          <tr>
            <td><span class="badge {r.status}">{label[r.status]}</span></td>
            <td class="num">{r.distance != null ? r.distance.toFixed(4) : ''}</td>
            <td title={r.path}>
              <div class="file">
                <img class="thumb" src={`/thumb/local?path=${encodeURIComponent(r.path)}`} alt="" loading="lazy" onerror={hide} />
                <span>{name(r.path)}</span>
              </div>
            </td>
            <td>
              <div class="file">
                {#if m}
                  {#if thumbs}<img class="thumb" src={`/thumb/asset/${m.assetId}`} alt="" loading="lazy" onerror={hide} />{/if}
                  <span>
                    {m.originalFileName || m.assetId}
                    {#if m.localDateTime}<br /><span class="muted">{m.localDateTime.slice(0, 10)}</span>{/if}
                  </span>
                {:else}
                  <span class="muted">{r.error ?? ''}</span>
                {/if}
              </div>
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
    gap: 1.5rem;
    flex-wrap: wrap;
    min-width: 0;
  }
  .svc {
    display: inline-flex;
    align-items: baseline;
    gap: 0.4rem;
    max-width: 26rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .dot {
    flex: none;
    width: 0.6rem;
    height: 0.6rem;
    border-radius: 50%;
    background: var(--muted);
    opacity: 0.6;
  }
  .dot.ok {
    background: var(--good);
    opacity: 1;
  }
  .dot.bad {
    background: var(--bad);
    opacity: 1;
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
  .settings h2,
  .settings .hint,
  .settings .wide,
  .settings .row {
    grid-column: 1 / -1;
  }
  .settings h2 {
    font-size: 0.95rem;
    font-weight: 600;
    margin: 0.5rem 0 0;
  }
  .settings h2:first-child {
    margin-top: 0;
  }
  .settings .hint {
    margin: -0.25rem 0 0;
    font-size: 0.85rem;
    color: var(--muted);
  }
  .settings .pick {
    display: flex;
    gap: 0.5rem;
  }
  .settings .pick input {
    flex: 1;
  }
  .settings .row {
    display: flex;
    gap: 1rem;
    align-items: center;
  }
  .file {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }
  .thumb {
    width: 72px;
    height: 72px;
    flex: none;
    object-fit: cover;
    border-radius: 4px;
    background: var(--stripe);
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
  .note {
    padding: 0.6rem 0.9rem;
    border-left: 3px solid var(--accent);
    border-radius: 4px;
    background: var(--stripe);
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
