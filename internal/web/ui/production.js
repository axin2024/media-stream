'use strict';

const productionView = {
  id: '', snapshot: null, slots: [], rundown: null, draft: [], draftChanged: false, drag: -1,
  cursor: '', monitorBank: 0, monitors: new Map(), scope: 0,
  pending: null, take: null, busy: false, poll: false, lease: null, players: new Map(),
  asRun: null, archive: null, archiveQuery: '', graphic: '',
};
const productionStatusLabels = {draft: '待播', on_air: '播出中', ending: '封存中', sealed: '已封存'};
const productionTakeLabels = {queued: '待编码', published: '已播出', fenced: '控制权已变更', failed: '媒体未能切换'};
const productionReasonLabels = {rundown: '串联单', director: '导播切换', source_unavailable: '源失联 · 备用垫片',
  rundown_gap: '编排留白 · 备用垫片', take_source_unavailable: '预选源不可用', source_render_failed: '源编码失败 · 备用垫片'};
const productionKindLabels = {live: '直播', clip: '片段', slate: '垫片', advert: '固定广告'};
const productionRoot = '/api/productions';
const productionVisible = () => !$('production-workbench').hidden && !!state.me;
const productionPath = () => productionRoot + '/' + encodeURIComponent(productionView.id);
const productionCanControl = () => role('owner', 'director', 'operator');
const productionCanGraph = () => role('owner', 'director', 'editor');
const productionCanEdit = () => role('owner', 'director');
const productionCanConfigure = () => role('owner', 'operator');

function productionElement(tag, value = '') {
  const node = document.createElement(tag);
  node.textContent = value;
  return node;
}

function productionFeedback(message, failure = false) {
  const node = $('production-status');
  node.textContent = message;
  node.dataset.alert = String(failure);
  node.hidden = !message;
}

function productionFields(form) {
  return Object.fromEntries(new FormData(form));
}

function productionTime(frame) {
  const second = Math.floor(frame / 25);
  return [Math.floor(second / 3600), Math.floor(second / 60) % 60, second % 60, frame % 25]
    .map(value => String(value).padStart(2, '0')).join(':');
}

function productionFrame(raw) {
  const parts = /^(\d{2}):([0-5]\d):([0-5]\d):([0-2]\d)$/.exec(raw.trim());
  if (!parts || Number(parts[4]) > 24 || Number(parts[1]) > 24) throw new Error('请输入有效的 25 帧时码，例如 00:00:05:00。');
  const [hour, minute, second, frame] = parts.slice(1).map(Number);
  return ((hour * 60 + minute) * 60 + second) * 25 + frame;
}

function bindProduction(name, event, callback) {
  $(name).addEventListener(event, async input => {
    if (event === 'submit') input.preventDefault();
    if (productionView.busy || !productionVisible() && name !== 'show-production') return;
    productionView.busy = true;
    const button = input.submitter ?? (input.currentTarget instanceof HTMLButtonElement ? input.currentTarget : null);
    if (button) button.disabled = true;
    productionFeedback('');
    try {
      await callback(input);
    } catch (error) {
      if (error.value?.current && productionView.snapshot) {
        productionView.lease = null;
        Object.assign(productionView.snapshot, error.value.current);
        productionRenderState();
      }
      if (error.value?.conflict_items?.length) {
        $('production-rundown-conflict').textContent = error.value.conflict_items
          .map(item => item.item_id + ' 与 ' + item.against_id + ' 的固定时序冲突（重叠 ' + item.overlap_frames + ' 帧）').join('。');
      }
      productionFeedback(error.value?.field ? error.value.field + ' · ' + error.message : error.message, true);
    } finally {
      productionView.busy = false;
      if (button) button.disabled = false;
      productionRenderState();
    }
  });
}

function productionTitle(id) {
  if (!id) return '—';
  const slot = productionView.slots.find(value => value.slot_id === id);
  return slot && slot.title !== id ? slot.title + ' · ' + id : id;
}

async function productionList(more = false) {
  const list = await api(productionRoot + (more ? '?after=' + encodeURIComponent(productionView.cursor) : ''));
  if (!productionVisible()) return;
  const select = $('production-select');
  const previous = Array.from(select.options).find(option => option.value === productionView.id);
  if (!more) select.replaceChildren(new Option('选择节目', ''));
  for (const item of list.items) select.add(new Option(item.title + ' · ' + item.channel, item.id));
  if (!Array.from(select.options).some(option => option.value === productionView.id) && previous) select.add(previous);
  select.value = productionView.id;
  productionView.cursor = list.next;
  $('production-more').hidden = !list.next;
}

function productionReleaseMonitors() {
  for (const monitor of productionView.monitors.values()) {
    monitor.video.pause();
    monitor.video.removeAttribute('src');
    if (monitor.url) URL.revokeObjectURL(monitor.url);
  }
  productionView.monitors.clear();
}

function productionStopPlayer(id) {
  const video = $(id);
  productionView.players.get(id)?.destroy();
  productionView.players.delete(id);
  video.pause();
  video.removeAttribute('src');
  video.load();
}

function productionReset() {
  ++productionView.scope;
  productionReleaseMonitors();
  productionStopPlayer('production-video');
  productionStopPlayer('production-archive-video');
  if (productionView.graphic) URL.revokeObjectURL(productionView.graphic);
  Object.assign(productionView, {id: '', snapshot: null, slots: [], rundown: null, draft: [], draftChanged: false,
    cursor: '', monitorBank: 0, pending: null, take: null, lease: null, asRun: null, archive: null, archiveQuery: '', graphic: ''});
  $('production-selected').hidden = true;
  $('production-graphic-preview').hidden = true;
  $('production-graphic-preview').removeAttribute('src');
  $('production-graphic-list').replaceChildren();
  $('production-select').replaceChildren(new Option('选择节目', ''));
  for (const id of ['production-player-status', 'production-take-result', 'production-rundown-conflict',
    'production-asrun-list', 'production-archive-status', 'production-archive-missing']) $(id).replaceChildren();
  for (const id of ['production-asrun-more', 'production-asrun-export', 'production-archive-play', 'production-archive-export',
    'production-archive-video']) $(id).hidden = true;
  $('production-archive-form').reset();
}

async function productionOpen(id) {
  if (id === productionView.id && productionView.snapshot) return;
  const select = $('production-select');
  const options = Array.from(select.options);
  const cursor = productionView.cursor;
  productionReset();
  select.replaceChildren(...options);
  productionView.cursor = cursor;
  productionView.id = id;
  select.value = id;
  if (!id) return;
  const ticket = productionView.scope;
  const path = productionPath();
  const [value, slots, rundown] = await Promise.all([api(path), api(path + '/slots'), api(path + '/rundown')]);
  if (ticket !== productionView.scope) return;
  productionView.snapshot = value;
  productionView.slots = slots;
  $('production-selected').hidden = false;
  productionSetRundown(rundown);
  productionRenderSlots();
  productionRenderState();
  if (productionCanGraph()) await productionLoadGraphics();
}

function productionHeld() {
  const value = productionView.snapshot;
  return !!(productionView.lease && value && productionView.lease.id === value.id &&
    productionView.lease.epoch === value.control_epoch && Date.parse(value.control_until) > Date.now());
}

function productionRenderState() {
  const value = productionView.snapshot;
  if (!value) return;
  const stage = value.state;
  const active = stage === 'on_air';
  const draft = stage === 'draft';
  const held = productionHeld();
  $('production-state').textContent = productionStatusLabels[stage] ?? stage;
  $('production-program').textContent = productionTitle(value.program_slot);
  $('production-preview-fact').textContent = productionTitle(value.preview_slot);
  $('production-frame').textContent = productionTime(value.program_frame);
  const next = productionView.rundown?.items.find(item => item.in_frame > value.program_frame);
  $('production-next').textContent = next ? next.title + ' · ' + productionTime(next.in_frame) : '—';
  $('production-since').textContent = value.started_at
    ? '开播时间 ' + new Intl.DateTimeFormat('zh-CN', {dateStyle: 'short', timeStyle: 'medium', timeZone: value.timezone}).format(new Date(value.started_at))
    : '时区 ' + value.timezone;
  $('production-controller').textContent = (value.controller || '待领取') + ' · 代际 ' + value.control_epoch + ' · 备用 ' + value.fallback_slot +
    (value.control_until ? ' · 剩余 ' + Math.max(0, Math.ceil((Date.parse(value.control_until) - Date.now()) / 1000)) + ' 秒' : '');
  const controlEnabled = draft || active;
  $('production-control').hidden = !productionCanControl();
  $('production-claim').disabled = !controlEnabled || !!(value.control_until && Date.parse(value.control_until) > Date.now() && !held && value.controller !== state.me?.name);
  $('production-start').disabled = !held || !draft;
  $('production-end').disabled = !held || !active;
  $('production-take').disabled = !held || !active || !value.preview_slot || productionView.take?.state === 'queued';
  $('production-play').disabled = value.program_frame === 0;
  $('production-slots').hidden = !productionCanConfigure();
  $('production-slot-form').hidden = !draft;
  $('production-clock-form').hidden = !controlEnabled;
  $('production-input-form').hidden = !controlEnabled;
  $('production-lost-form').hidden = !active;
  $('production-rundown-editor').hidden = !productionCanEdit() || !controlEnabled;
  $('production-graphics').hidden = !productionCanGraph();
  $('production-graphic-form').hidden = !controlEnabled;
  $('production-graphic-clear').hidden = !controlEnabled;
  $('production-archive-form').hidden = stage !== 'sealed' || value.program_frame === 0;
  if (stage === 'sealed' && !$('production-archive-form').elements.to.value) {
    $('production-archive-form').elements.to.value = productionTime(value.program_frame);
  }
  $('production-rundown-rev').textContent = '版本 ' + value.rundown_rev;
  $('production-asrun-rev').textContent = '版本 ' + value.asrun_rev;
  for (const [id, monitor] of productionView.monitors) {
    monitor.card.dataset.program = String(value.program_slot === id);
    monitor.card.dataset.preview = String(value.preview_slot === id);
    if (monitor.preview) monitor.preview.disabled = !held || !controlEnabled;
  }
}

function productionFillSlots(select, slots) {
  const prior = select.value;
  select.replaceChildren(...slots.map(slot => new Option(productionTitle(slot.slot_id), slot.slot_id)));
  if (slots.some(slot => slot.slot_id === prior)) select.value = prior;
}

function productionRenderSlots() {
  const live = productionView.slots.filter(slot => slot.kind === 'live');
  productionFillSlots($('production-clock-form').elements.slot, live);
  productionFillSlots($('production-lost-form').elements.slot, live);
  productionFillSlots($('production-input-form').elements.slot, productionView.slots);
  const banks = Math.ceil(live.length / 4);
  const bank = $('production-monitor-page');
  bank.replaceChildren(...Array.from({length: banks}, (_, index) => new Option(String(index+1), String(index))));
  $('production-monitor-bank').hidden = banks <= 1;
  bank.value = String(productionView.monitorBank);
  const subset = live.slice(productionView.monitorBank * 4, productionView.monitorBank * 4 + 4);
  const expected = subset.map(slot => slot.slot_id).join(',');
  const existing = Array.from(productionView.monitors.keys()).join(',');
  if (expected !== existing || $('production-monitors').children.length === 0) {
    productionReleaseMonitors();
    $('production-monitors').replaceChildren(...subset.map(slot => productionMonitorCard(slot)));
    if (subset.length === 0) $('production-monitors').append(productionElement('p', '未配置直播机位。'));
  }
  $('production-monitor-refresh').hidden = !productionCanControl();
  for (const slot of subset) {
    const monitor = productionView.monitors.get(slot.slot_id);
    monitor.info.textContent = slot.timecode_zero_ms == null ? '待校时' :
      (slot.lost_at ? '失联 · ' : '') + '偏移 ' + slot.alignment_ms + ' ms · ' + (slot.last_source_frame == null ? '等待分片' : '帧 ' + slot.last_source_frame);
  }
}

function productionMonitorCard(slot) {
  const id = slot.slot_id;
  const card = productionElement('article');
  card.dataset.slot = id;
  const title = productionElement('h3', productionTitle(id));
  const video = productionElement('video');
  video.autoplay = true;
  video.loop = true;
  video.muted = true;
  video.playsInline = true;
  video.setAttribute('aria-label', id + ' 预监');
  const info = productionElement('p');
  const toolbar = productionElement('div');
  toolbar.className = 'buttons';
  const monitor = {card, video, info, url: '', last: null, preview: null};
  if (productionCanControl()) {
    const listen = productionElement('button', '试听');
    listen.type = 'button';
    listen.className = 'secondary';
    listen.setAttribute('aria-pressed', 'false');
    listen.addEventListener('click', () => {
      const listening = video.muted;
      for (const item of productionView.monitors.values()) {
        item.video.muted = item.video !== video || !listening;
        item.listen?.setAttribute('aria-pressed', String(!item.video.muted));
      }
      if (listening) video.play().catch(() => productionFeedback('预监尚无可播媒体。', true));
    });
    monitor.listen = listen;
    const preview = productionElement('button', '设为预监');
    preview.type = 'button';
    preview.className = 'secondary';
    preview.addEventListener('click', async () => {
      if (preview.disabled) return;
      preview.disabled = true;
      try {
        await productionCommand('preview', {slot_id: id});
      } catch (error) {
        productionFeedback(error.message, true);
      } finally {
        productionRenderState();
      }
    });
    monitor.preview = preview;
    toolbar.append(listen, preview);
  }
  card.append(title, video, info, toolbar);
  productionView.monitors.set(id, monitor);
  return card;
}

async function productionMedia(path, options = {}) {
  const headers = new Headers(options.headers);
  headers.set('Authorization', 'Bearer ' + state.token);
  const response = await fetch(path, {...options, headers, cache: 'no-store'});
  if (!response.ok) {
    const error = await response.json().catch(() => ({}));
    throw Object.assign(new Error(error.message || '媒体请求失败'), {status: response.status, value: error});
  }
  return response.blob();
}

async function productionRefreshMonitors() {
  const scope = productionView.scope;
  const path = productionPath();
  await Promise.all(productionView.slots.filter(slot => productionView.monitors.has(slot.slot_id)).map(async slot => {
    const item = productionView.monitors.get(slot.slot_id);
    if (slot.last_source_frame == null || item.last === slot.last_source_frame) return;
    try {
      const blob = await productionMedia(path + '/slots/' + encodeURIComponent(slot.slot_id) + '/monitor.mp4');
      if (scope !== productionView.scope || productionView.monitors.get(slot.slot_id) !== item) return;
      if (item.url) URL.revokeObjectURL(item.url);
      item.url = URL.createObjectURL(blob);
      item.last = slot.last_source_frame;
      item.video.src = item.url;
      await item.video.play().catch(() => {});
    } catch (error) {
      if (scope === productionView.scope) item.info.textContent += ' · ' + error.message;
    }
  }));
}

async function productionCommand(action, extra = {}) {
  const scope = productionView.scope;
  const snapshot = productionView.snapshot;
  const result = await api(productionPath() + '/' + action, json('POST', {expected_control_epoch: snapshot.control_epoch, ...extra}));
  if (scope !== productionView.scope) return;
  if (action === 'control') productionView.lease = {id: result.id, epoch: result.control_epoch};
  productionView.snapshot = result;
  productionRenderState();
}

async function productionRefresh() {
  if (!productionVisible() || !productionView.id || productionView.poll || productionView.busy) return;
  const scope = productionView.scope;
  const path = productionPath();
  productionView.poll = true;
  try {
    const value = await api(path);
    if (scope !== productionView.scope || productionView.busy) return;
    const old = productionView.snapshot;
    if (old && value.asrun_rev < old.asrun_rev) return;
    productionView.snapshot = value;
    if (!productionView.draftChanged && value.rundown_rev !== productionView.rundown?.rundown_rev) {
      const rundown = await api(path + '/rundown');
      if (scope !== productionView.scope || productionView.draftChanged || productionView.busy) return;
      productionSetRundown(rundown);
    }
    if (value.state === 'sealed' && old?.state !== 'sealed') productionRenderRundown();
    if (value.state === 'on_air' || value.state !== old?.state) {
      const slots = await api(path + '/slots');
      if (scope !== productionView.scope || productionView.busy) return;
      productionView.slots = slots;
      productionRenderSlots();
      if (productionCanControl() && Array.from(productionView.monitors.values()).some(item => item.url)) await productionRefreshMonitors();
    }
    if (scope !== productionView.scope || productionView.busy) return;
    if (productionView.take?.state === 'queued') {
      const take = await api(path + '/takes/' + productionView.take.take_id);
      if (scope !== productionView.scope) return;
      productionView.take = take;
      productionRenderTake();
    }
    productionRenderState();
  } catch (error) {
    if (scope === productionView.scope) productionFeedback('状态更新中断，保留上一条已确认画面。' + error.message, true);
  } finally {
    productionView.poll = false;
  }
}

function productionRenderTake() {
  const take = productionView.take;
  $('production-take-result').textContent = take.take_id + ' · ' + (productionTakeLabels[take.state] ?? take.state) +
    (take.actual_pts == null ? '' : ' · ' + productionTime(take.actual_frame) + ' · PTS ' + take.actual_pts);
}

function productionSetRundown(rundown) {
  productionView.rundown = rundown;
  productionView.draft = rundown.items.map(item => ({...item}));
  productionView.draftChanged = false;
  $('production-rundown-conflict').textContent = '';
  productionRenderRundown();
}

function productionMoveSegment(from, to) {
  const items = productionView.draft;
  if (from === to || !items[from] || !items[to]) return;
  const start = Math.min(from, to);
  const end = Math.max(from, to);
  if (items.slice(start, end + 1).some(item => item.fixed)) {
    productionFeedback('固定段落保持原位，只能调整同一固定窗口内的段落。', true);
    return;
  }
  const gaps = items.slice(start, end).map((item, index) => items[start + index + 1].in_frame - item.out_frame);
  let position = items[start].in_frame;
  const [moving] = items.splice(from, 1);
  items.splice(to, 0, moving);
  for (let index = start; index <= end; index++) {
    const item = items[index];
    const span = item.out_frame - item.in_frame;
    item.in_frame = position;
    item.out_frame = position + span;
    position = item.out_frame + (gaps[index-start] ?? 0);
  }
  productionView.draftChanged = true;
  productionRenderRundown();
}

function productionRundownField(row, item, field, label, choices) {
  const node = productionElement('label', label);
  const input = productionElement(choices ? 'select' : 'input');
  input.required = true;
  const timeField = ['in_frame', 'out_frame', 'source_in_frame'].includes(field);
  if (choices) input.replaceChildren(...choices);
  input.value = timeField ? productionTime(item[field]) : item[field];
  input.addEventListener('input', () => {
    productionView.draftChanged = true;
    if (!timeField) {
      item[field] = input.value;
      return;
    }
    try {
      item[field] = productionFrame(input.value);
      input.setCustomValidity('');
    } catch (error) {
      input.setCustomValidity(error.message);
    }
  });
  node.append(input);
  row.append(node);
}

function productionRenderRundown() {
  const list = $('production-rundown-list');
  const editable = productionCanEdit() && ['draft', 'on_air'].includes(productionView.snapshot.state);
  const sourceChoices = () => productionView.slots.map(slot => new Option(productionTitle(slot.slot_id), slot.slot_id));
  list.replaceChildren(...productionView.draft.map((item, index) => {
    const row = productionElement('li');
    row.dataset.item = item.id;
    if (!editable) {
      row.textContent = productionTime(item.in_frame) + ' – ' + productionTime(item.out_frame) + ' · ' + item.title +
        ' · ' + productionTitle(item.slot_id) + (item.fixed ? ' · 固定' : '');
      return row;
    }
    row.draggable = !item.fixed;
    row.addEventListener('dragstart', event => {
      productionView.drag = index;
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('text/plain', item.id);
    });
    row.addEventListener('dragover', event => {
      if (productionView.drag >= 0 && productionView.drag !== index && !item.fixed) event.preventDefault();
    });
    row.addEventListener('drop', event => {
      event.preventDefault();
      if (productionView.drag >= 0) productionMoveSegment(productionView.drag, index);
      productionView.drag = -1;
    });
    row.addEventListener('dragend', () => { productionView.drag = -1; });
    productionRundownField(row, item, 'title', '名称');
    productionRundownField(row, item, 'slot_id', '槽位', sourceChoices());
    productionRundownField(row, item, 'kind', '类型', Object.entries(productionKindLabels).map(([key, value]) => new Option(value, key)));
    productionRundownField(row, item, 'in_frame', '播入');
    productionRundownField(row, item, 'out_frame', '播出');
    productionRundownField(row, item, 'source_in_frame', '源时码');
    const locked = productionElement('label', '固定时刻');
    const checkbox = productionElement('input');
    checkbox.type = 'checkbox';
    checkbox.checked = item.fixed;
    checkbox.addEventListener('change', () => {
      item.fixed = checkbox.checked;
      row.draggable = !item.fixed;
      productionView.draftChanged = true;
      productionRenderRundown();
    });
    locked.append(checkbox);
    const toolbar = productionElement('div');
    toolbar.className = 'buttons';
    for (const [label, offset] of [['上移', -1], ['下移', 1], ['移除', 0]]) {
      const control = productionElement('button', label);
      control.type = 'button';
      control.className = 'secondary';
      control.setAttribute('aria-label', item.title + ' · ' + label);
      control.disabled = offset !== 0 && (item.fixed || !productionView.draft[index + offset] || productionView.draft[index + offset].fixed);
      control.addEventListener('click', () => {
        if (offset) {
          productionMoveSegment(index, index + offset);
        } else {
          productionView.draft.splice(index, 1);
          productionView.draftChanged = true;
          productionRenderRundown();
        }
      });
      toolbar.append(control);
    }
    row.append(locked, toolbar);
    return row;
  }));
  if (!list.children.length) list.append(productionElement('li', '没有已编排段落。'));
  const extend = $('production-extension-form').elements.namedItem('item');
  extend.replaceChildren(...productionView.rundown.items.filter(item => !item.fixed && item.kind === 'live')
    .map(item => new Option(item.title, item.id)));
}

async function productionUpdateRundown(extra) {
  const path = productionPath();
  const scope = productionView.scope;
  $('production-rundown-conflict').textContent = '';
  const draft = await api(path + '/rundown', json('PUT', {expected_revision: productionView.rundown.rundown_rev, ...extra}));
  if (scope !== productionView.scope) return;
  productionSetRundown(draft);
  productionView.snapshot.rundown_rev = draft.rundown_rev;
  productionFeedback('已确认节目单版本 ' + draft.rundown_rev + '。');
}

async function productionLoadGraphics() {
  const scope = productionView.scope;
  const overlay = await api(productionPath() + '/overlays');
  if (scope !== productionView.scope) return;
  $('production-graphic-list').replaceChildren(...overlay.items
    .sort((a, b) => a.on_frame - b.on_frame)
    .map(item => productionElement('li', productionTime(item.on_frame) + ' – ' + productionTime(item.off_frame) +
      ' · ' + item.text + ' · ' + item.overlay_id + (item.cleared_by ? ' · 手工清屏 ' + item.cleared_by : ''))));
}

async function productionReadAsRun(more = false) {
  const scope = productionView.scope;
  const after = more ? productionView.asRun.next_frame : 0;
  const page = await api(productionPath() + '/as-run?from_frame=' + after);
  if (scope !== productionView.scope) return;
  if (more) page.items = [...productionView.asRun.items, ...page.items];
  productionView.asRun = page;
  $('production-asrun-more').hidden = page.next_frame == null;
  $('production-asrun-export').hidden = !page.items.length;
  $('production-asrun-list').replaceChildren(...page.items.map(entry => {
    const row = productionElement('li');
    const published = entry.published_at != null;
    row.dataset.start = entry.actual_in_frame;
    row.dataset.end = entry.actual_out_frame;
    row.dataset.reason = entry.plan.reason;
    const label = productionElement('strong', productionTime(entry.actual_in_frame) + ' – ' + productionTime(entry.actual_out_frame));
    const detail = productionElement('span', (productionReasonLabels[entry.plan.reason] ?? entry.plan.reason) +
      ' · ' + productionTitle(entry.plan.source.slot_id));
    const align = productionElement('small', '计划 ' + productionTime(entry.plan.planned_in_frame) + ' · ' +
      (published ? '编码分片 ' + entry.segment_sequence + (entry.take_id ? ' · TAKE ' + entry.take_id : '') : '等待编码确认'));
    row.append(label, detail, align);
    return row;
  }));
}

function productionDownload(filename, value) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(value, null, 2) + '\n'], {type: 'application/json'}));
  const link = productionElement('a');
  link.href = url;
  link.download = filename;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

async function productionPlay(id, url, status) {
  productionStopPlayer(id);
  await api('/api/playback-cookie', {method: 'POST'});
  const video = $(id);
  video.hidden = false;
  if (globalThis.Hls?.isSupported()) {
    const hls = new Hls({enableWorker: true});
    productionView.players.set(id, hls);
    hls.on(Hls.Events.MANIFEST_PARSED, () => {
      if (status) $(status).textContent = '播放列表已载入。';
      video.play().catch(() => {});
    });
    hls.on(Hls.Events.ERROR, (_, detail) => {
      if (detail.fatal && status) $(status).textContent = '播放列表或媒体暂不可用。';
    });
    hls.loadSource(url);
    hls.attachMedia(video);
  } else if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = url;
    await video.play().catch(() => {});
  } else {
    productionFeedback('此浏览器无法播放 HLS。', true);
  }
}

document.addEventListener('studio:login', () => {
  $('show-production').hidden = !role('owner', 'director', 'operator', 'editor', 'viewer');
  $('production-create').hidden = !role('owner');
});
document.addEventListener('studio:logout', () => {
  productionReset();
  $('show-production').setAttribute('aria-pressed', 'false');
  $('show-studio').setAttribute('aria-pressed', 'true');
  $('live-workbench').hidden = false;
  $('production-workbench').hidden = true;
});

$('show-studio').addEventListener('click', () => {
  $('show-production').setAttribute('aria-pressed', 'false');
  $('show-studio').setAttribute('aria-pressed', 'true');
  $('live-workbench').hidden = false;
  $('production-workbench').hidden = true;
  productionStopPlayer('production-video');
  productionStopPlayer('production-archive-video');
  for (const item of productionView.monitors.values()) item.video.pause();
});

bindProduction('show-production', 'click', async () => {
  $('show-production').setAttribute('aria-pressed', 'true');
  $('show-studio').setAttribute('aria-pressed', 'false');
  $('production-workbench').hidden = false;
  $('live-workbench').hidden = true;
  player.pause();
  await productionList();
});
bindProduction('production-refresh', 'click', () => productionList());
bindProduction('production-more', 'click', () => productionList(true));
bindProduction('production-select', 'change', event => productionOpen(event.target.value));
bindProduction('production-create-form', 'submit', async event => {
  const value = await mutation(productionRoot, productionFields(event.currentTarget));
  await productionOpen(value.id);
  await productionList();
  $('production-create').open = false;
});

bindProduction('production-monitor-refresh', 'click', async () => {
  for (const item of productionView.monitors.values()) item.video.play().catch(() => {});
  await productionRefreshMonitors();
});
bindProduction('production-monitor-page', 'change', async event => {
  productionView.monitorBank = Number(event.target.value);
  productionRenderSlots();
  if (productionCanControl()) await productionRefreshMonitors();
});
bindProduction('production-claim', 'click', () => productionCommand('control', {lease_seconds: 120}));
bindProduction('production-start', 'click', () => productionCommand('start'));
bindProduction('production-end', 'click', () => productionCommand('end'));
bindProduction('production-play', 'click', () => {
  const value = productionView.snapshot;
  const kind = value.state === 'sealed' ? 'vod' : 'live';
  return productionPlay('production-video', '/media/' + kind + '/' + encodeURIComponent(value.program_output) + '/index.m3u8', 'production-player-status');
});
bindProduction('production-take-form', 'submit', async event => {
  const value = productionView.snapshot;
  const fields = productionFields(event.currentTarget);
  const body = {slot_id: value.preview_slot, expected_control_epoch: value.control_epoch, transition: fields.transition,
    transition_frames: fields.transition === 'cut' ? 0 : Number(fields.transition_frames)};
  const signature = JSON.stringify(body);
  if (productionView.pending?.signature !== signature) productionView.pending = {signature, key: unique(), command: unique()};
  const pending = productionView.pending;
  const ticket = productionView.scope;
  const take = await api(productionPath() + '/takes', json('POST', {...body, command_id: pending.command}, pending.key));
  if (ticket !== productionView.scope) return;
  productionView.pending = null;
  productionView.take = take;
  productionRenderTake();
});

async function productionReloadSlots() {
  const scope = productionView.scope;
  const slots = await api(productionPath() + '/slots');
  if (scope !== productionView.scope) return;
  productionView.slots = slots;
  productionRenderSlots();
}

bindProduction('production-slot-form', 'submit', async event => {
  const {slot, ...body} = productionFields(event.currentTarget);
  const value = await api(productionPath() + '/slots/' + encodeURIComponent(slot), json('PUT', body));
  await productionReloadSlots();
  productionFeedback('槽位 ' + value.slot_id + ' 已保存。');
});
bindProduction('production-clock-form', 'submit', async event => {
  const fields = productionFields(event.currentTarget);
  const body = {samples: [1, 2, 3].map(index => ({source_time_ms: Number(fields['source' + index]), program_time_ms: Number(fields['program' + index])}))};
  const value = await api(productionPath() + '/slots/' + encodeURIComponent(fields.slot) + '/clock', json('PUT', body));
  await productionReloadSlots();
  productionFeedback(value.slot_id + ' 的校准偏移为 ' + value.alignment_ms + ' ms。');
});
bindProduction('production-input-form', 'submit', async event => {
  const fields = productionFields(event.currentTarget);
  const value = await api(productionPath() + '/slots/' + encodeURIComponent(fields.slot) + '/input/' + encodeURIComponent(fields.frame),
    {method: 'PUT', body: fields.file, headers: {'Content-Type': 'video/mp2t', 'X-Frame-Count': fields.frames}});
  await productionReloadSlots();
  productionFeedback((value.replay ? '已确认重复片段。' : '片段已收录。') + '节目首帧 ' + value.start_frame + ' · 帧数 ' + value.frame_count);
});
bindProduction('production-lost-form', 'submit', async event => {
  const {slot} = productionFields(event.currentTarget);
  await api(productionPath() + '/slots/' + encodeURIComponent(slot) + '/lost', {method: 'POST'});
  await productionReloadSlots();
  productionFeedback(slot + ' 已标记为失联。');
});

bindProduction('production-rundown-add', 'click', () => {
  const slot = productionView.slots[0];
  if (!slot) throw new Error('请先配置节目输入槽位。');
  const previous = productionView.draft.at(-1);
  const frame = previous?.out_frame ?? 0;
  productionView.draft.push({id: 'item-' + unique(), title: '新段落', slot_id: slot.slot_id, kind: slot.kind,
    in_frame: frame, out_frame: frame+125, source_in_frame: 0, fixed: false});
  productionView.draftChanged = true;
  productionRenderRundown();
});
bindProduction('production-rundown-reset', 'click', async () => productionSetRundown(await api(productionPath() + '/rundown')));
bindProduction('production-rundown-save', 'click', () => {
  for (const field of $('production-rundown-list').querySelectorAll('input, select')) {
    if (!field.reportValidity()) return;
  }
  return productionUpdateRundown({items: productionView.draft});
});
bindProduction('production-extension-form', 'submit', event => {
  if (productionView.draftChanged) throw new Error('保存或重载本地修改后再延长已确认的节目单。');
  const {item, seconds} = productionFields(event.currentTarget);
  return productionUpdateRundown({extend_item: item, extension_frames: Number(seconds)*25});
});

bindProduction('production-graphic-form', 'submit', async event => {
  const fields = productionFields(event.currentTarget);
  const input = {template: fields.template, text: fields.text, font_size: Number(fields.font_size),
    duration_frames: Number(fields.duration_frames)};
  if (fields.on_frame) input.on_frame = productionFrame(fields.on_frame);
  if (event.submitter?.value !== 'publish') {
    const graphic = await productionMedia(productionPath() + '/overlays/preview.png', json('POST', input));
    if (productionView.graphic) URL.revokeObjectURL(productionView.graphic);
    productionView.graphic = URL.createObjectURL(graphic);
    $('production-graphic-preview').src = productionView.graphic;
    $('production-graphic-preview').hidden = false;
    return;
  }
  const result = await api(productionPath() + '/overlays', json('POST', input));
  productionFeedback('图层已安排 · ' + result.overlay_id + ' · ' + productionTime(result.off_frame) + ' 撤下。');
  await productionLoadGraphics();
});
bindProduction('production-graphic-clear', 'click', async () => {
  const result = await api(productionPath() + '/overlays/clear', {method: 'POST'});
  productionFeedback('将在 ' + productionTime(result.off_frame) + ' 清屏。');
  await productionLoadGraphics();
});

bindProduction('production-asrun-load', 'click', () => productionReadAsRun());
bindProduction('production-asrun-more', 'click', () => productionReadAsRun(true));
bindProduction('production-asrun-export', 'click', async () => {
  const ticket = productionView.scope;
  let read = await api(productionPath() + '/as-run');
  const entries = [...read.items];
  const rev = read.asrun_rev;
  while (read.next_frame != null) {
    read = await api(productionPath() + '/as-run?from_frame=' + read.next_frame);
    if (scopeChanged() || rev !== read.asrun_rev) throw new Error('实际播出单在导出时有更新，请重新导出。');
    entries.push(...read.items);
  }
  if (!scopeChanged()) productionDownload('as-run-' + productionView.id + '.json', {...read, items: entries});
  function scopeChanged() { return ticket !== productionView.scope; }
});
bindProduction('production-archive-form', 'submit', async event => {
  const fields = productionFields(event.currentTarget);
  const from = productionFrame(fields.from);
  const to = productionFrame(fields.to);
  if (to <= from) throw new Error('结束时码需晚于开始时码。');
  productionStopPlayer('production-archive-video');
  $('production-archive-video').hidden = true;
  const query = new URLSearchParams({from_frame: from, to_frame: to, track: fields.track});
  const result = await api(productionPath() + '/archive?' + query);
  productionView.archive = result;
  productionView.archiveQuery = query;
  const complete = result.state === 'complete' && !!result.archive_digest;
  $('production-archive-status').textContent = complete
    ? '完整 · ' + productionTime(result.actual_from_frame) + ' – ' + productionTime(result.actual_to_frame) + ' · SHA-256 ' + result.archive_digest
    : '所选区间存在缺片。';
  $('production-archive-status').dataset.digest = result.archive_digest ?? '';
  $('production-archive-missing').replaceChildren(...result.missing.map(item =>
    productionElement('li', productionTime(item.from_frame) + ' – ' + productionTime(item.to_frame) + ' · ' +
      ((item.to_frame - item.from_frame) / 25).toFixed(2) + ' 秒 · ' + item.source)));
  $('production-archive-play').hidden = !complete;
  $('production-archive-export').hidden = false;
});
bindProduction('production-archive-play', 'click', () => productionPlay('production-archive-video',
  '/media/archive/' + encodeURIComponent(productionView.id) + '/index.m3u8?' + productionView.archiveQuery, 'production-player-status'));
bindProduction('production-archive-export', 'click', () => productionDownload('archive-' + productionView.id + '.json', productionView.archive));
setInterval(productionRefresh, 2000);
