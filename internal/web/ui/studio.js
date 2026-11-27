'use strict';

const $ = (id) => document.getElementById(id);
const player = $('player');
const state = {
  token: '', me: null, channel: null, recording: null, session: null, selection: 0, hls: null,
  channelCursor: '', recordingCursor: '', editCursor: '', eventCursor: 0, actions: new Map(),
  device: unique(), playback: null, refreshing: false,
  batchTargets: new Map(), batchDraft: null, batch: null, batchCursor: '',
};
const labels = {
  waiting: '等待输入', live: '直播中', degraded: '转码降级', switching: '切换中', ending: '归档中',
  archived: '已归档', published: '已发布', hidden: '已下架', empty: '无媒体', purged: '已清理',
  queued: '排队中', running: '处理中', failed: '失败', primary: '主路', backup: '备路',
};
const eventLabels = {
  session_opened: '打开直播会话', input_requested: '请求切换输入', input_selected: '选中输入',
  input_timeout: '检测到输入停顿', input_lost: '推流器报告断开', input_rotated: '轮换输入凭据',
  transcode_failed: '转码失败', segment_committed: '分片已可播', session_ending: '开始归档',
  recording_archived: '回放归档', recording_visibility: '回放可见范围更新',
  recording_expired: '回放到期', recording_purged: '回收回放引用', caption_added: '字幕已导入',
  edit_queued: '剪辑排队', edit_result: '剪辑结果',
};

function unique() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (value) => value.toString(16).padStart(2, '0')).join('');
}

function note(text, error = false) {
  $('notice').textContent = text;
  $('notice').dataset.alert = String(error);
}

async function api(path, options = {}) {
  const headers = new Headers(options.headers);
  headers.set('Authorization', 'Bearer ' + state.token);
  const response = await fetch(path, {...options, headers, cache: 'no-store'});
  if (response.status === 204) return;
  const value = await response.json();
  if (!response.ok) throw Object.assign(new Error(value.message || '请求失败'), {status: response.status, value});
  return value;
}

function json(method, body, key) {
  const headers = {'Content-Type': 'application/json'};
  if (key) headers['Idempotency-Key'] = key;
  return {method, body: JSON.stringify(body), headers};
}

async function mutation(path, input) {
  const operation = path + JSON.stringify(input);
  if (!state.actions.has(operation)) state.actions.set(operation, unique());
  const result = await api(path, json('POST', input, state.actions.get(operation)));
  state.actions.delete(operation);
  return result;
}

function point(root, label, callback) {
  const button = document.createElement('button');
  button.type = 'button';
  button.textContent = label;
  button.dataset.entry = 'true';
  runOn(button, 'click', async () => {
    if (button.getAttribute('aria-pressed') === 'true') return;
    for (const selected of document.querySelectorAll('[data-entry][aria-pressed=true]')) selected.setAttribute('aria-pressed', 'false');
    button.setAttribute('aria-pressed', 'true');
    await callback();
  });
  const row = document.createElement('li');
  row.append(button);
  root.append(row);
  return row;
}

function rawText(node, value) {
  const li = document.createElement('li');
  li.textContent = value;
  node.append(li);
  return li;
}

function seconds(ms) {
  return Number.isFinite(ms) ? (ms / 1000).toLocaleString('zh-CN', {maximumFractionDigits: 3}) + ' 秒' : '—';
}

function bytes(value) {
  if (value >= 1048576) return (value / 1048576).toFixed(1) + ' MiB';
  if (value >= 1024) return (value / 1024).toFixed(1) + ' KiB';
  return value + ' B';
}

function role(...roles) {
  return roles.includes(state.me?.role);
}

function resetPlayer() {
  state.playback = null;
  if (state.hls) state.hls.destroy();
  state.hls = null;
  player.pause();
  player.removeAttribute('src');
  player.load();
  $('quality-level').replaceChildren(new Option('自动', '-1'));
  $('quality-level').disabled = true;
  $('player-stats').textContent = '';
}

async function refreshDirectory(reset = true, part = 'all') {
  if (part !== 'recordings') {
    const page = await api('/api/channels?after=' + encodeURIComponent(reset ? '' : state.channelCursor));
    if (reset) $('channels').replaceChildren();
    for (const item of page.items) point($('channels'), item.title, () => select(item, null));
    if (reset && !page.items.length) rawText($('channels'), '当前没有频道');
    state.channelCursor = page.next;
    $('channels-more').hidden = !page.next;
  }
  if (part !== 'channels') {
    const page = await api('/api/recordings?before=' + encodeURIComponent(reset ? '' : state.recordingCursor));
    if (reset) $('recordings').replaceChildren();
    for (const item of page.items) {
      const when = new Date(item.created_at).toLocaleDateString('zh-CN');
      const row = point($('recordings'), item.channel_title + ' · ' + when + ' · ' + labels[item.state], () => select(null, item));
      if (role('owner', 'editor') && (item.state === 'published' || state.batchTargets.has(item.id))) {
        const check = document.createElement('input');
        check.type = 'checkbox';
        check.checked = state.batchTargets.has(item.id);
        check.dataset.batchRecording = item.id;
        check.setAttribute('aria-label', '选择回放 ' + item.channel_title + ' ' + item.id);
        check.addEventListener('change', () => {
          if (!check.checked) state.batchTargets.delete(item.id);
          else if (state.batchTargets.size < 50) state.batchTargets.set(item.id, item);
          else {
            check.checked = false;
            note('一个批次最多选择 50 个回放。', true);
          }
          renderVisibilitySelection();
        });
        row.prepend(check);
      }
    }
    if (reset && !page.items.length) rawText($('recordings'), '当前没有回放');
    state.recordingCursor = page.next;
    $('recordings-more').hidden = !page.next;
  }
}

function renderVisibilitySelection() {
  const selection = $('visibility-batch-selection');
  selection.replaceChildren();
  for (const rec of state.batchTargets.values()) rawText(selection, rec.channel_title + ' · ' + rec.id + ' · 版本 ' + rec.revision);
  $('visibility-batch-count').textContent = '已选 ' + state.batchTargets.size + ' 个回放';
  $('visibility-batch-prepare').textContent = state.batchDraft ? '重试保存原选集' : '保存选集';
}

function visibilityBatchPath(key) {
  return '/api/visibility-batches/' + encodeURIComponent(key);
}

function renderVisibilityBatch(batch) {
  state.batch = batch;
  $('visibility-batch-receipts').replaceChildren();
  $('visibility-batch-execute').hidden = !batch || batch.state === 'completed';
  $('visibility-batch-status').textContent = batch
    ? '批次 ' + batch.request_key + ' · ' + (batch.hide ? '下架' : '设置到期时间') + ' · 已处理 ' + batch.processed + ' / ' + batch.item_count
    : '';
  if (!batch) return;
  const reasons = {
    not_found: '找不到可操作的回放',
    revision_or_state_changed: '版本或状态已变化',
    deadline_elapsed: '指定的截止时间已过',
  };
  for (const item of batch.items) {
    let result = '未开始';
    if (item.state === 'applied') result = '已完成，版本 ' + item.applied_revision;
    if (item.state === 'rejected') result = '已拒绝，' + reasons[item.reason];
    rawText($('visibility-batch-receipts'), item.recording_id + ' · 预期版本 ' + item.expected_revision + ' · ' + result);
  }
}

async function refreshVisibilityBatches(reset = true) {
  if (!role('owner', 'editor')) return;
  const identity = state.me;
  const path = '/api/visibility-batches?before=' + encodeURIComponent(reset ? '' : state.batchCursor);
  const page = await api(path);
  if (state.me !== identity) return;
  const history = $('visibility-batch-history');
  if (reset) history.replaceChildren(new Option('选择批次', ''));
  const known = new Set([...history.options].map((option) => option.value));
  for (const batch of page.items) {
    if (!known.has(batch.request_key)) {
      history.add(new Option((batch.hide ? '下架' : '到期') + ' · ' + batch.request_key + ' · ' +
        batch.processed + '/' + batch.item_count, batch.request_key));
    }
  }
  history.value = state.batch?.request_key || '';
  if (history.selectedIndex < 0) history.selectedIndex = 0;
  state.batchCursor = page.next;
  $('visibility-batch-more').hidden = !page.next;
}

function clearVisibilitySelection() {
  state.batchTargets.clear();
  for (const check of $('recordings').querySelectorAll('[data-batch-recording]')) check.checked = false;
  renderVisibilitySelection();
}

async function reloadVisibilityOutcome(path, mutationOptions) {
  try {
    return await api(path + (mutationOptions.method === 'POST' ? '/execute' : ''), mutationOptions);
  } catch (error) {
    if (error.status && error.status < 500) throw error;
    try {
      return await api(path);
    } catch {
      throw error;
    }
  }
}

async function select(channel, recording) {
  state.selection++;
  state.channel = channel;
  state.recording = recording;
  state.session = null;
  state.eventCursor = 0;
  state.editCursor = '';
  resetPlayer();
  for (const id of ['events', 'quality-summary', 'edits']) $(id).replaceChildren();
  for (const id of ['events-more', 'edits-more', 'grants']) $(id).hidden = true;
  $('grants').textContent = '';
  renderSelection();
  await refreshSelection(true);
}

function renderSelection() {
  const stream = state.session;
  const channel = state.channel;
  const recording = state.recording;
  $('selection-type').textContent = recording ? '回放' : '频道';
  $('selection-title').textContent = recording?.channel_title || channel?.title || '选择频道或回放';
  $('state').textContent = labels[recording?.state || stream?.state] || (channel ? '已预约' : '待选择');
  $('watch').disabled = !stream || stream.next_sequence === 0 || (recording && recording.state !== 'published') ||
    (channel && stream.state === 'archived');
  $('stream-facts').hidden = !stream;
  if (stream) {
    $('active-source').textContent = labels[stream.active_source] || '待检测';
    $('source-epoch').textContent = stream.source_epoch;
    $('sequence').textContent = stream.next_sequence;
    $('media-duration').textContent = seconds(stream.media_duration_ms);
  }
  $('open-form').hidden = !(channel && role('owner') && channel.owner === state.me.name && (!stream || stream.state === 'archived'));
  $('live-actions').hidden = !(channel && stream && role('owner', 'director') && !['ending', 'archived'].includes(stream.state));
  $('recording-actions').hidden = !(recording?.state === 'published' && role('owner', 'editor'));
  $('operation-empty').hidden = !($('open-form').hidden && $('live-actions').hidden && $('recording-actions').hidden);
}

async function refreshSelection(collectDetails = false) {
  const generation = state.selection;
  const session = state.recording?.session_id || state.channel?.session_id;
  if (!session) return;
  const value = await api('/api/sessions/' + session);
  if (generation !== state.selection) return;
  state.session = value;
  renderSelection();
  if (role('owner', 'director', 'editor', 'operator')) {
    await Promise.all([recentEvents(session, generation), quality(session, generation)]);
  }
  if (!state.recording || !collectDetails || generation !== state.selection) return;
  await listEdits(true, generation);
  if (state.recording.state === 'published') await listCaptions(generation);
}

async function quality(id, generation) {
  const value = await api('/api/sessions/' + id + '/quality');
  if (generation !== state.selection) return;
  const pairs = [
    ['源切换', value.switches], ['排队或转码中', value.waiting_jobs], ['失败作业', value.failed_jobs],
    ['平均起播', value.starts ? seconds(Math.round(value.startup_ms / value.starts)) : '—'],
    ['播放端卡顿', value.stalls + ' 次 · ' + seconds(value.stalled_ms)], ['播放端错误', value.player_errors],
    ['入口流量', bytes(value.ingress_bytes)], ['出口流量', bytes(value.egress_bytes)], ['出口错误', value.egress_errors],
  ];
  const target = $('quality-summary');
  target.replaceChildren();
  for (const [label, text] of pairs) {
    const term = document.createElement('dt');
    const value = document.createElement('dd');
    term.textContent = label;
    value.textContent = text;
    target.append(term, value);
  }
}

async function recentEvents(id, generation) {
  const items = await api('/api/sessions/' + id + '/events?after=' + state.eventCursor);
  if (generation !== state.selection) return;
  const root = $('events');
  for (const item of items) {
    if (item.id <= state.eventCursor) continue;
    const line = rawText(root, eventLabels[item.kind] || item.kind);
    const date = document.createElement('small');
    const details = document.createElement('code');
    date.textContent = new Date(item.at).toLocaleTimeString('zh-CN') + ' · ' + item.actor;
    details.textContent = JSON.stringify(item.details);
    line.append(date, details);
    state.eventCursor = item.id;
  }
  while (root.children.length > 250) root.firstElementChild.remove();
  $('events-more').hidden = items.length < 200;
}

async function listEdits(reset, generation) {
  const rec = state.recording;
  if (!rec) return;
  const page = await api('/api/recordings/' + rec.id + '/edits?after=' + encodeURIComponent(reset ? '' : state.editCursor));
  if (state.selection !== generation) return;
  const output = $('edits');
  if (reset) output.replaceChildren();
  for (const item of page.items) {
    const line = rawText(output, seconds(item.from_ms) + ' → ' + seconds(item.to_ms) + ' · ' + labels[item.state]);
    if (item.state === 'published') {
      if (item.actual_from_ms !== item.from_ms || item.actual_to_ms !== item.to_ms) {
        line.append(document.createTextNode(' · 实际 ' + seconds(item.actual_from_ms) + ' → ' + seconds(item.actual_to_ms)));
      }
      for (const [title, file] of [['MP4', 'video.mp4'], ...(item.has_captions ? [['WebVTT', 'subtitles.vtt']] : [])]) {
        const link = document.createElement('a');
        link.textContent = title;
        link.href = '/media/clip/' + item.id + '/' + file;
        link.download = file;
        line.append(link);
      }
    }
    if (item.error) line.append(document.createTextNode(' · ' + item.error));
  }
  state.editCursor = page.next;
  $('edits-more').hidden = !page.next;
}

async function listCaptions(generation) {
  const items = await api('/api/recordings/' + state.recording.id + '/captions');
  if (state.selection !== generation) return;
  const list = $('edit-caption');
  const previous = list.value;
  list.replaceChildren(new Option('无', ''));
  for (const item of items) list.add(new Option(item.language + ' · v' + item.revision, item.id));
  if (items.some((item) => item.id === previous)) list.value = previous;
}

async function operations() {
  $('ops').hidden = !role('owner', 'operator');
  if ($('ops').hidden) return;
  const amounts = await api('/api/ops/usage');
  $('usage').replaceChildren();
  for (const row of amounts) {
    rawText($('usage'), new Date(row.hour).toLocaleString('zh-CN') + ' · ' + bytes(row.ingress_bytes) +
      ' 输入 · ' + bytes(row.egress_bytes) + ' 输出 · ' + row.media_requests + ' 请求');
  }
  $('nodes').replaceChildren();
  if (role('operator') && !state.me.channel) {
    const values = await api('/api/ops/nodes');
    for (const node of values) rawText($('nodes'), node.id + ' · ' + node.slots + ' 并发 · ' + (node.enabled ? '已启用' : '已禁用') +
      ' · ' + new Date(node.last_seen).toLocaleTimeString('zh-CN'));
  }
}

function runOn(node, event, fn) {
  node.addEventListener(event, async (input) => {
    if (event === 'submit') input.preventDefault();
    const button = event === 'submit' ? input.submitter : node.tagName === 'BUTTON' ? node : null;
    if (button) button.disabled = true;
    try {
      await fn(input);
    } catch (error) {
      note(error.message, true);
    } finally {
      if (button) button.disabled = false;
      if (node === $('watch')) renderSelection();
    }
  });
}

runOn($('login'), 'submit', async () => {
  state.token = $('token').value.trim();
  const identity = await api('/api/me');
  await api('/api/playback-cookie', {method: 'POST'});
  state.me = identity;
  $('token').value = '';
  $('login').hidden = true;
  $('workspace').hidden = false;
  $('identity').hidden = false;
  $('identity-text').textContent = state.me.tenant + ' · ' + state.me.name;
  $('channel-form-section').hidden = !role('owner');
  const canBatch = role('owner', 'editor');
  $('visibility-batches').hidden = !canBatch;
  $('visibility-batch-link').hidden = !canBatch;
  const from = new Date(Date.now() - 60_000);
  const to = new Date(Date.now() + 3_600_000);
  const local = (date) => new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
  $('channel-form').elements.from.value = local(from);
  $('channel-form').elements.until.value = local(to);
  renderVisibilitySelection();
  await Promise.all([refreshDirectory(), operations(), refreshVisibilityBatches()]);
  document.dispatchEvent(new Event('studio:login'));
  note('已连接。选择节目查看媒体和事件。');
});

runOn($('sign-out'), 'click', async () => {
  await api('/api/playback-cookie', {method: 'DELETE'});
  state.token = '';
  state.me = null;
  document.dispatchEvent(new Event('studio:logout'));
  state.actions.clear();
  state.batchDraft = null;
  state.batchCursor = '';
  clearVisibilitySelection();
  renderVisibilityBatch(null);
  $('visibility-batch-history').replaceChildren(new Option('选择批次', ''));
  await select(null, null);
  $('identity').hidden = true;
  $('workspace').hidden = true;
  $('login').hidden = false;
  note('已退出。');
});

runOn($('reload'), 'click', async () => {
  await refreshDirectory();
  await operations();
  await refreshSelection(true);
  note('目录已刷新。');
});
runOn($('channels-more'), 'click', () => refreshDirectory(false, 'channels'));
runOn($('recordings-more'), 'click', () => refreshDirectory(false, 'recordings'));
runOn($('visibility-batch-form').elements.action, 'change', () => {
  const deadline = $('visibility-batch-form').elements.action.value === 'deadline';
  $('visibility-batch-deadline').hidden = !deadline;
  $('visibility-batch-form').elements.until.required = deadline;
});
runOn($('visibility-batch-clear'), 'click', clearVisibilitySelection);
runOn($('visibility-batch-refresh'), 'click', async () => {
  const identity = state.me;
  const key = state.batch?.request_key;
  if (key) {
    const latest = await api(visibilityBatchPath(key));
    if (state.me !== identity) return;
    if (state.batch?.request_key === key) renderVisibilityBatch(latest);
  }
  await refreshVisibilityBatches();
});
runOn($('visibility-batch-more'), 'click', () => refreshVisibilityBatches(false));
runOn($('visibility-batch-history'), 'change', async () => {
  const identity = state.me;
  const key = $('visibility-batch-history').value;
  const batch = key ? await api(visibilityBatchPath(key)) : null;
  if (state.me === identity && $('visibility-batch-history').value === key) renderVisibilityBatch(batch);
});
runOn($('visibility-batch-form'), 'submit', async () => {
  if (!state.batchDraft) {
    if (!state.batchTargets.size) throw new Error('请在回放目录选择至少一个回放。');
    const form = $('visibility-batch-form').elements;
    const hide = form.action.value === 'hide';
    const items = [...state.batchTargets.values()].map((rec) => ({recording_id: rec.id, expected_revision: rec.revision}));
    const input = {items, hide};
    if (!hide) input.visible_until = new Date(form.until.value).toISOString();
    state.batchDraft = {key: unique(), input};
  }
  const identity = state.me;
  const draft = state.batchDraft;
  renderVisibilitySelection();
  let batch;
  try {
    batch = await reloadVisibilityOutcome(visibilityBatchPath(draft.key), json('PUT', draft.input));
  } catch (error) {
    if (state.me === identity && error.status && error.status < 500) {
      state.batchDraft = null;
      renderVisibilitySelection();
    }
    throw error;
  }
  if (state.me !== identity) return;
  state.batchDraft = null;
  clearVisibilitySelection();
  renderVisibilityBatch(batch);
  await refreshVisibilityBatches();
  note('选集已保存，可以检查版本后再执行。');
});
runOn($('visibility-batch-execute'), 'click', async () => {
  if (!state.batch || state.batch.state === 'completed') return;
  const identity = state.me;
  const key = state.batch.request_key;
  const batch = await reloadVisibilityOutcome(visibilityBatchPath(key), {method: 'POST'});
  if (state.me !== identity || state.batch?.request_key !== key) return;
  renderVisibilityBatch(batch);
  if (batch.hide && batch.items.some((item) => item.state === 'applied' && item.recording_id === state.recording?.id)) {
    resetPlayer();
    state.recording = await api('/api/recordings/' + state.recording.id);
    renderSelection();
  }
  await Promise.all([refreshVisibilityBatches(), refreshDirectory(true, 'recordings')]);
  note(batch.state === 'completed' ? '批次已完成，请核对逐项回执。' : '已取回原回执，可继续处理剩余项目。');
});
runOn($('edits-more'), 'click', () => listEdits(false, state.selection));
runOn($('events-more'), 'click', () => recentEvents(state.session.id, state.selection));

runOn($('channel-form'), 'submit', async (event) => {
  const form = new FormData(event.currentTarget || $('channel-form'));
  const input = {
    key: form.get('key'), title: form.get('title'), available_from: new Date(form.get('from')).toISOString(),
    available_until: new Date(form.get('until')).toISOString(),
  };
  const channel = await api('/api/channels', json('POST', input));
  await refreshDirectory();
  await select(channel, null);
  note('频道已预约。');
});

runOn($('open-form'), 'submit', async () => {
  const form = new FormData($('open-form'));
  const selected = state.channel;
  const response = await mutation('/api/channels/' + encodeURIComponent(selected.key) + '/sessions', {
    preset: form.get('preset'), segment_ms: Number(form.get('segment')) * 1000, primary_source_zero_ms: Number(form.get('primary')),
    backup_source_zero_ms: Number(form.get('backup')), push_ttl_seconds: Number(form.get('ttl')) * 60,
  });
  await select({...selected, session_id: response.session.id}, null);
  $('grants').textContent = JSON.stringify(response.inputs, null, 2);
  $('grants').hidden = false;
  note('已打开会话 ' + response.session.id + '。请私下交付两路推流凭据。');
});

async function prefer(source) {
  const current = state.session;
  await api('/api/sessions/' + current.id + '/prefer-input', json('POST', {source, revision: current.revision}));
  await refreshSelection();
  note('已登记切换请求，目标输入需有可解码分片。');
}
runOn($('prefer-primary'), 'click', () => prefer('primary'));
runOn($('prefer-backup'), 'click', () => prefer('backup'));
runOn($('end-stream'), 'click', async () => {
  await api('/api/sessions/' + state.session.id + '/end', json('POST', {}));
  $('grants').textContent = '';
  $('grants').hidden = true;
  await refreshSelection();
  note('已开始归档，稍后在回放目录刷新查看。');
});

runOn($('rotation-form'), 'submit', async () => {
  const form = new FormData($('rotation-form'));
  const source = form.get('source');
  const response = await mutation('/api/sessions/' + state.session.id + '/inputs/' + source + '/rotate',
    {generation: Number(form.get('generation')), source_zero_ms: Number(form.get('clock')), push_ttl_seconds: 7200});
  $('grants').textContent = JSON.stringify(response, null, 2);
  $('grants').hidden = false;
  $('rotation-form').elements.generation.value = response.generation;
  note(labels[source] + '凭据已轮换。');
});

runOn($('caption-form'), 'submit', async () => {
  const form = new FormData($('caption-form'));
  const file = form.get('subtitle');
  await api('/api/recordings/' + state.recording.id + '/captions/' + encodeURIComponent(form.get('lang')),
    {method: 'POST', body: file, headers: {'Content-Type': 'text/vtt', 'Idempotency-Key': unique()}});
  await listCaptions(state.selection);
  note('字幕已导入，重新播放即可选择。');
});

runOn($('edit-form'), 'submit', async () => {
  const form = new FormData($('edit-form'));
  const rec = state.recording;
  const value = await mutation('/api/recordings/' + rec.id + '/edits', {recording_revision: rec.revision,
    from_ms: Math.round(Number(form.get('from')) * 1000), to_ms: Math.round(Number(form.get('to')) * 1000),
    caption_id: form.get('caption')});
  await listEdits(true, state.selection);
  note('剪辑 ' + value.id + ' 已排队。');
});

async function visibility(hide, until) {
  const rec = state.recording;
  state.recording = await api('/api/recordings/' + rec.id + '/visibility',
    json('POST', {revision: rec.revision, hide, visible_until: until}));
  if (hide) resetPlayer();
  renderSelection();
  await refreshDirectory();
  note(hide ? '回放已下架，已经交付的剪辑按自身期限继续可读。' : '回放可见期限已更新。');
}
runOn($('hide-recording'), 'click', () => visibility(true, null));
runOn($('visibility-form'), 'submit', () => visibility(false, new Date($('visibility-form').elements.until.value).toISOString()));

runOn($('watch'), 'click', async () => {
  resetPlayer();
  const session = state.session;
  const url = '/media/' + (state.recording ? 'vod' : 'live') + '/' + session.id + '/index.m3u8';
  state.playback = {started: performance.now(), session: session.id, playing: false, stall: 0, stalls: 0, total: 0, errors: 0};
  if (window.Hls?.isSupported()) {
    const hls = new Hls({enableWorker: true, liveSyncDurationCount: 2, liveMaxLatencyDurationCount: 6});
    state.hls = hls;
    hls.attachMedia(player);
    hls.loadSource(url);
    hls.on(Hls.Events.MANIFEST_PARSED, (_type, data) => {
      if (state.hls !== hls) return;
      const levels = $('quality-level');
      levels.replaceChildren(new Option('自动', '-1'));
      data.levels.forEach((level, index) => levels.add(new Option(level.height ? level.height + 'p' : '源画质', index)));
      levels.disabled = data.levels.length < 2;
    });
    hls.on(Hls.Events.ERROR, (_type, details) => {
      if (state.hls !== hls || !details.fatal) return;
      const value = state.playback;
      if (value && value.errors === 0) {
        value.errors++;
        recordSignal(value, 'error', 0);
      }
      note('播放器错误，请刷新后重试。', true);
    });
  } else if (player.canPlayType('application/vnd.apple.mpegurl')) {
    player.src = url;
  } else {
    note('当前浏览器不支持 HLS 播放。', true);
    return;
  }
  // Browser autoplay policies also allow using the visible native play control.
  player.play().catch(() => {});
});

$('quality-level').addEventListener('change', () => {
  if (state.hls) state.hls.currentLevel = Number($('quality-level').value);
});

function recordSignal(playback, kind, duration) {
  const body = {device: state.device, event_key: unique(), kind, duration_ms: Math.round(duration),
    media_ms: Math.round(Math.max(0, player.currentTime || 0) * 1000)};
  const route = '/api/sessions/' + playback.session + '/quality';
  api(route, json('POST', body)).catch((error) => {
    if (!state.token || (error.status && error.status < 500)) return;
    api(route, json('POST', body)).catch(() => {});
  });
}

player.addEventListener('waiting', () => {
  const current = state.playback;
  if (!current?.playing || player.paused || player.seeking || current.stall) return;
  current.stall = performance.now();
});
player.addEventListener('seeking', () => {
  if (state.playback) state.playback.stall = 0;
});
player.addEventListener('pause', () => {
  if (state.playback) state.playback.stall = 0;
});
player.addEventListener('playing', () => {
  const current = state.playback;
  if (!current) return;
  if (!current.playing) {
    current.playing = true;
    const startup = performance.now() - current.started;
    recordSignal(current, 'startup', startup);
    $('player-stats').textContent = '起播 ' + seconds(Math.round(startup));
  }
  if (current.stall) {
    const length = performance.now() - current.stall;
    current.stall = 0;
    current.stalls++;
    current.total += length;
    recordSignal(current, 'stall', length);
    $('player-stats').textContent = '卡顿 ' + current.stalls + ' 次 · ' + seconds(Math.round(current.total));
  }
});
player.addEventListener('ended', () => {
  if (state.playback) recordSignal(state.playback, 'ended', 0);
});

setInterval(async () => {
  if (!state.me || state.refreshing || document.hidden) return;
  state.refreshing = true;
  try {
    await refreshSelection();
    if (state.recording && state.editCursor === '') await listEdits(true, state.selection);
  } catch (error) {
    if (error.status && error.status !== 404) note(error.message, true);
  } finally {
    state.refreshing = false;
  }
}, 3000);

setInterval(async () => {
  if (!state.me) return;
  try {
    await api('/api/playback-cookie', {method: 'POST'});
  } catch (error) {
    if (error.status === 403) note('访问凭据已到期，请退出后重新连接。', true);
  }
}, 20 * 60 * 1000);
