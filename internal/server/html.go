package server

import "net/http"

// handleForm serves the browser annotation form. It is deliberately minimal (no
// styling framework, no template engine): three input modes (a single locus, a
// batch of loci, or a VCF upload), the known annotations as checkboxes fetched
// from /ui/annotations (defaults pre-checked, with select-all/none), and vanilla
// JS that submits a job, polls its status, renders the result as a table, and
// offers JSON/CSV/TSV downloads. Batch and VCF modes post to /ui/submit/vcf.
func (s *Server) handleForm(w http.ResponseWriter, r *http.Request) {
	s.ensureSession(w, r) // give the browser a session id so it can list its history
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(formHTML))
}

const formHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>VariantHub — annotate variants</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; max-width: 900px; }
  h1 { font-size: 1.3rem; }
  fieldset { margin: 1rem 0; }
  label.ann { display: block; font-size: 0.9rem; }
  .src { margin: 0.5rem 0; }
  .src > strong { font-size: 0.85rem; color: #444; }
  input[type=text] { width: 22rem; padding: 0.3rem; font-family: monospace; }
  textarea { width: 100%; height: 8rem; padding: 0.3rem; font-family: monospace; box-sizing: border-box; }
  button { padding: 0.4rem 0.9rem; }
  .modes label { margin-right: 1rem; font-size: 0.95rem; }
  .toolbar { margin: 0.3rem 0; }
  .toolbar button { padding: 0.15rem 0.5rem; font-size: 0.8rem; }
  table { border-collapse: collapse; margin-top: 1rem; }
  th, td { border: 1px solid #ccc; padding: 0.3rem 0.6rem; text-align: left; vertical-align: top; }
  th { background: #f4f4f4; }
  #status { margin-top: 1rem; font-style: italic; color: #555; }
  #downloads { margin-top: 1rem; }
  #downloads[hidden] { display: none; }
  .err { color: #b00; }
  code { background: #f4f4f4; padding: 0 0.2rem; }
  .hint { font-size: 0.85rem; color: #666; }
</style>
</head>
<body>
<h1>VariantHub — annotate variants</h1>
<p id="snapshot-info" class="hint"></p>

<form id="form">
  <div class="modes">
    <label><input type="radio" name="mode" value="single" checked> Single locus</label>
    <label><input type="radio" name="mode" value="batch"> Batch loci</label>
    <label><input type="radio" name="mode" value="vcf"> VCF file</label>
  </div>

  <div id="mode-single" class="mode">
    <input type="text" id="locus" name="locus" placeholder="chr1:115256529:T:C" autofocus>
    <p class="hint">A variant locus as <code>chrom:pos:ref:alt</code> (POS is 1-based).</p>
  </div>

  <div id="mode-batch" class="mode" hidden>
    <textarea id="loci" placeholder="chr1:115256529:T:C&#10;chr2:200:C:T"></textarea>
    <p class="hint">One <code>chrom:pos:ref:alt</code> per line.</p>
  </div>

  <div id="mode-vcf" class="mode" hidden>
    <input type="file" id="vcf" accept=".vcf,.vcf.gz">
    <p class="hint">A VCF file (sites-only is fine). Multi-allelic ALTs are split per allele.</p>
  </div>

  <fieldset id="annset">
    <legend>Annotations</legend>
    <div class="toolbar">
      <button type="button" id="sel-all">Select all</button>
      <button type="button" id="sel-none">Select none</button>
    </div>
    <div id="anns">loading…</div>
  </fieldset>

  <button type="submit">Annotate</button>
</form>

<div id="status"></div>
<div id="downloads" hidden>
  <button type="button" id="dl-json">Download JSON</button>
  <button type="button" id="dl-csv">Download CSV</button>
  <button type="button" id="dl-tsv">Download TSV</button>
</div>
<div id="results"></div>

<h2 style="font-size:1.05rem; margin-top:2.5rem;">Recent requests
  <button type="button" id="hist-refresh" style="font-size:0.75rem; padding:0.1rem 0.4rem; margin-left:0.5rem;">refresh</button>
</h2>
<p class="hint">Your prior submissions from this browser session. Click a completed one to view its results again.</p>
<div id="history">loading…</div>

<script>
const $ = (id) => document.getElementById(id);
let lastVariants = null;

async function loadAnnotations() {
  const r = await fetch('/ui/annotations');
  const data = await r.json();
  const title = data.title || data.snapshot;
  $('snapshot-info').textContent = 'Snapshot: ' + title +
    (data.assembly ? ' (' + data.assembly + ')' : '');
  const box = $('anns');
  box.innerHTML = '';
  for (const src of data.sources) {
    if (!src.annotations.length) continue;
    const div = document.createElement('div');
    div.className = 'src';
    const v = src.version ? (':' + src.version) : '';
    const label = src.title ? (src.title + ' — ' + src.name + v) : (src.name + v);
    div.innerHTML = '<strong>' + label + ' (' + src.type + ')</strong>';
    for (const a of src.annotations) {
      const label = document.createElement('label');
      label.className = 'ann';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.value = a.name;
      cb.className = 'annbox';
      cb.checked = a.default;
      label.appendChild(cb);
      const desc = a.description ? (' — ' + a.description) : '';
      label.appendChild(document.createTextNode(' ' + a.name + desc));
      div.appendChild(label);
    }
    box.appendChild(div);
  }
}

function selectedAnnotations() {
  return Array.from(document.querySelectorAll('.annbox'))
    .filter(cb => cb.checked).map(cb => cb.value);
}

function currentMode() {
  return document.querySelector('input[name=mode]:checked').value;
}

function showMode() {
  const m = currentMode();
  for (const id of ['single', 'batch', 'vcf']) {
    $('mode-' + id).hidden = (id !== m);
  }
}

// Build a minimal sites-only VCF from chrom:pos:ref:alt lines (for batch mode).
function lociToVCF(text) {
  const rows = [];
  for (let line of text.split('\n')) {
    line = line.trim();
    if (!line) continue;
    const p = line.split(':');
    if (p.length < 4) throw new Error('bad locus (need chrom:pos:ref:alt): ' + line);
    rows.push([p[0], p[1], '.', p[2], p[3], '.', '.', '.'].join('\t'));
  }
  if (!rows.length) throw new Error('no loci entered');
  return '##fileformat=VCFv4.2\n#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n' + rows.join('\n') + '\n';
}

function renderTable(variants) {
  lastVariants = variants;
  $('downloads').hidden = !variants || !variants.length;
  const box = $('results');
  box.innerHTML = '';
  for (const v of variants) {
    const h = document.createElement('h3');
    h.textContent = v.chrom + ':' + v.pos + ':' + v.ref + ':' + v.alt;
    box.appendChild(h);
    const table = document.createElement('table');
    table.innerHTML = '<tr><th>annotation</th><th>value</th></tr>';
    const keys = Object.keys(v.annotations);
    if (!keys.length) {
      const tr = document.createElement('tr');
      tr.innerHTML = '<td colspan="2"><em>no annotations selected</em></td>';
      table.appendChild(tr);
    }
    for (const k of keys) {
      const tr = document.createElement('tr');
      const val = v.annotations[k];
      const cell = (val === null || val === undefined) ? '' : String(val);
      const td1 = document.createElement('td'); td1.textContent = k;
      const td2 = document.createElement('td'); td2.textContent = cell;
      tr.appendChild(td1); tr.appendChild(td2);
      table.appendChild(tr);
    }
    box.appendChild(table);
  }
}

async function poll(jobId) {
  for (;;) {
    const r = await fetch('/ui/jobs/' + jobId);
    const job = await r.json();
    if (job.status === 'done') {
      $('status').textContent = 'Done (' + job.n_variants + ' variant(s)).';
      const rr = await fetch('/ui/jobs/' + jobId + '/results');
      renderTable(await rr.json());
      loadHistory();
      return;
    }
    if (job.status === 'error') {
      $('status').innerHTML = '<span class="err">Job failed: ' + (job.error || 'unknown error') + '</span>';
      loadHistory();
      return;
    }
    $('status').textContent = 'Status: ' + job.status + '…';
    await new Promise(res => setTimeout(res, 500));
  }
}

// --- request history (scoped to this browser's session) --------------------

function fmtTime(sec) {
  if (!sec) return '';
  return new Date(sec * 1000).toLocaleString();
}

async function loadHistory() {
  const box = $('history');
  try {
    const r = await fetch('/ui/jobs?limit=25');
    const data = await r.json();
    const jobs = data.jobs || [];
    if (!jobs.length) { box.innerHTML = '<p class="hint">No requests yet.</p>'; return; }
    const table = document.createElement('table');
    table.innerHTML = '<tr><th>when</th><th>request</th><th>kind</th><th>status</th><th>variants</th><th></th></tr>';
    for (const j of jobs) {
      const tr = document.createElement('tr');
      const td = (txt) => { const c = document.createElement('td'); c.textContent = txt; return c; };
      tr.appendChild(td(fmtTime(j.created_at)));
      tr.appendChild(td(j.label || ''));
      tr.appendChild(td(j.kind));
      tr.appendChild(td(j.status));
      tr.appendChild(td(j.status === 'done' ? String(j.n_variants) : ''));
      const act = document.createElement('td');
      if (j.status === 'done') {
        const b = document.createElement('button');
        b.type = 'button'; b.textContent = 'view';
        b.addEventListener('click', () => viewJob(j.job_id));
        act.appendChild(b);
      } else if (j.status === 'error') {
        act.textContent = '⚠'; act.title = j.error || 'failed';
      }
      tr.appendChild(act);
      table.appendChild(tr);
    }
    box.innerHTML = '';
    box.appendChild(table);
  } catch (e) {
    box.innerHTML = '<p class="err">could not load history</p>';
  }
}

async function viewJob(id) {
  $('status').textContent = 'Loading results for job ' + id + '…';
  const r = await fetch('/ui/jobs/' + id + '/results');
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    $('status').innerHTML = '<span class="err">' + (d.error || 'could not load results') + '</span>';
    return;
  }
  $('status').textContent = '';
  renderTable(await r.json());
  $('results').scrollIntoView({ behavior: 'smooth' });
}

// Submit the current mode's input, returning the parsed response (or throwing).
// ?wait= asks the server to hold the response briefly so fast jobs come back
// already done, letting us skip polling and jump straight to results.
const WAIT = 10;
async function submitJob() {
  const anns = selectedAnnotations();
  const mode = currentMode();
  if (mode === 'single') {
    const body = { locus: $('locus').value.trim(), annotations: anns };
    const r = await fetch('/ui/submit?wait=' + WAIT, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    return { r, data: await r.json() };
  }
  // batch + vcf both post multipart to /ui/submit/vcf.
  const fd = new FormData();
  fd.append('annotations', anns.join(','));
  if (mode === 'batch') {
    const vcf = lociToVCF($('loci').value);
    fd.append('vcf', new Blob([vcf], { type: 'text/plain' }), 'batch.vcf');
  } else {
    const f = $('vcf').files[0];
    if (!f) throw new Error('choose a VCF file');
    fd.append('vcf', f, f.name);
  }
  const r = await fetch('/ui/submit/vcf?wait=' + WAIT, { method: 'POST', body: fd });
  return { r, data: await r.json() };
}

function download(name, text, type) {
  const blob = new Blob([text], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = name;
  document.body.appendChild(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}

// Union of annotation keys across all variants, first-seen order.
function annotationColumns(variants) {
  const seen = new Set(), cols = [];
  for (const v of variants) for (const k of Object.keys(v.annotations)) {
    if (!seen.has(k)) { seen.add(k); cols.push(k); }
  }
  return cols;
}

function toDelimited(variants, sep) {
  const cols = annotationColumns(variants);
  const header = ['chrom', 'pos', 'ref', 'alt', ...cols];
  const esc = (s) => {
    s = (s === null || s === undefined) ? '' : String(s);
    if (sep === ',' && /[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
    if (sep === '\t') return s.replace(/[\t\n]/g, ' ');
    return s;
  };
  const lines = [header.map(esc).join(sep)];
  for (const v of variants) {
    const row = [v.chrom, v.pos, v.ref, v.alt, ...cols.map(c => v.annotations[c])];
    lines.push(row.map(esc).join(sep));
  }
  return lines.join('\n') + '\n';
}

document.querySelectorAll('input[name=mode]').forEach(el => el.addEventListener('change', showMode));
$('sel-all').addEventListener('click', () => document.querySelectorAll('.annbox').forEach(cb => cb.checked = true));
$('sel-none').addEventListener('click', () => document.querySelectorAll('.annbox').forEach(cb => cb.checked = false));
$('dl-json').addEventListener('click', () => lastVariants && download('varhub.json', JSON.stringify(lastVariants, null, 2), 'application/json'));
$('dl-csv').addEventListener('click', () => lastVariants && download('varhub.csv', toDelimited(lastVariants, ','), 'text/csv'));
$('dl-tsv').addEventListener('click', () => lastVariants && download('varhub.tsv', toDelimited(lastVariants, '\t'), 'text/tab-separated-values'));

$('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('results').innerHTML = '';
  $('downloads').hidden = true;
  $('status').textContent = 'Submitting…';
  try {
    const { r, data } = await submitJob();
    if (!r.ok) {
      $('status').innerHTML = '<span class="err">' + (data.error || 'submit failed') + '</span>';
      return;
    }
    // Fast path: the job finished within the server's wait buffer — render now.
    if (data.status === 'done' && data.results) {
      $('status').textContent = 'Done (' + (data.n_variants || data.results.length) + ' variant(s)).';
      renderTable(data.results);
      loadHistory();
      return;
    }
    if (data.status === 'error') {
      $('status').innerHTML = '<span class="err">Job failed: ' + (data.error || 'unknown error') + '</span>';
      loadHistory();
      return;
    }
    // Slow path: still running after the buffer — fall back to polling.
    poll(data.job_id);
  } catch (err) {
    $('status').innerHTML = '<span class="err">' + err.message + '</span>';
  }
});

$('hist-refresh').addEventListener('click', loadHistory);

showMode();
loadAnnotations();
loadHistory();
</script>
</body>
</html>
`
