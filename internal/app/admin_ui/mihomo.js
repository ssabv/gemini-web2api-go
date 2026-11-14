// Node optimization is a peer of the proxy and cookie pool pages.
const MihomoPage = (() => {
  let dispose = () => {};
  function unmount() {
    dispose();
    dispose = () => {};
  }
  function render() {
    return `<div id="mihomoPage">
<div class="panel"><h2>控制器连接</h2><div class="mo-grid">
<div class="mo-field"><label for="mihomo_controller">控制器地址</label><input id="mihomo_controller" type="url" placeholder="http://mihomo:9090"><p class="rthint">填写 external-controller 地址。</p></div>
<div class="mo-field"><label for="controllerSecret">控制器密码</label><input id="controllerSecret" type="password" autocomplete="new-password"><p class="rthint" id="secretState">密码单独保存，不回显。</p></div>
<div class="mo-field"><label for="mihomo_group">Gemini 策略组</label><select id="mihomo_group"><option value="">连接后选择策略组</option></select><p class="rthint">仅支持手动选择组（Selector）。</p></div>
<div class="mo-field"><label for="mihomo_proxy_url">对应代理地址</label><input id="mihomo_proxy_url" type="url" placeholder="http://mihomo:7890"><p class="rthint">须与代理池地址一致，Gemini 域名须经过上面的策略组。</p></div>
</div><div class="mo-actions"><button class="btn" id="connect" type="button">连接并获取策略组</button><button id="saveSecret" class="btn muted" type="button">保存密码</button><button id="clearSecret" class="btn muted" type="button">清除面板密码</button><span id="connectionMsg" role="status"></span></div></div>
<div class="panel"><h2>自动优选</h2>
<label class="mo-toggle"><input id="mihomo_auto_optimize" type="checkbox">定时选择最快节点并匿名验证</label>
<p class="rthint">读取 mihomo 已有测速结果 → 从低延迟节点开始验证 → 收到 Gemini 回复后保留。优选不触发重新测速。匿名测试固定发送“Reply with OK only.”，不使用账号 Cookie。全部失败恢复原节点。</p>
<div class="mo-grid" style="margin-top:20px">
<div class="mo-field"><label for="mihomo_optimize_interval_min">检测间隔（分钟）</label><input id="mihomo_optimize_interval_min" type="number" min="1" max="1440" value="5"><p class="rthint">默认 5 分钟；启用后约 10 秒内开始首轮。</p></div>
<div class="mo-field"><label for="mihomo_probe_timeout_sec">单节点匿名测试超时（秒）</label><input id="mihomo_probe_timeout_sec" type="number" min="5" max="60" value="20"><p class="rthint">不改变正常对话请求的超时。</p></div>
<div class="mo-field"><label for="mihomo_max_switches">每轮最多尝试节点数</label><input id="mihomo_max_switches" type="number" min="1" max="5" value="2"></div>
<div class="mo-field"><label for="mihomo_cooldown_sec">两轮切换最小间隔（秒）</label><input id="mihomo_cooldown_sec" type="number" min="0" max="300" value="5"></div>
<div class="mo-field mo-wide"><label for="mihomo_nodes">限定候选节点（可选）</label><textarea id="mihomo_nodes" placeholder="留空使用组内全部独立节点；多个节点用换行或 | 分隔"></textarea><p class="rthint">跳过子组、直连、拒绝和测不通的节点。地区不支持的节点暂避 30 分钟。</p></div>
</div><div style="margin-top:20px"><label class="mo-toggle"><input id="mihomo_auto_switch" type="checkbox">正式请求遇到地区限制时自动换节点</label><label class="mo-toggle"><input id="mihomo_prefer_low_latency" type="checkbox">故障恢复也优先低延迟节点</label></div>
<div class="mo-actions"><button class="btn" id="save" type="button">保存并生效</button><button class="btn" id="run" type="button">立即选最快并匿名验证</button><span id="saveMsg" role="status"></span></div>
<p class="rthint">立即验证使用已保存的配置，成功后会切换并保留该节点。</p>
</div>
<div class="panel"><h2>运行状态</h2><div id="state" class="mo-status" role="status">正在读取…</div></div>
<div class="panel"><h2>已有测速结果</h2><button id="readCached" class="btn muted" type="button">读取已有结果</button><button id="latency" class="btn muted" type="button">重新测速（不切换、不调用模型）</button><span id="latencyMsg" role="status"></span><p class="rthint">优选使用 mihomo 最近一次的结果；没有测速记录或最近一次测速失败的节点跳过。只有点击「重新测速」才发起批量测速。</p><div class="table-scroll"><table><thead><tr><th>节点</th><th>延迟</th><th>选择状态</th></tr></thead><tbody id="nodes"><tr><td colspan="3">正在读取已有测速结果。</td></tr></tbody></table></div></div>
<div class="panel"><h2>最近检测记录</h2><p class="rthint">显示本次启动以来最近 20 轮；不保存测试回复正文。</p><div class="table-scroll"><table><thead><tr><th>检测时间</th><th>结果 / 原因</th><th>最终节点</th><th>延迟 / 请求耗时</th></tr></thead><tbody id="history"></tbody></table></div></div>
</div>`;
  }
  async function mount() {
    unmount();
    const root = document.getElementById('mihomoPage');
    if (!root) return;
    let active = true;

    const $ = (id) => root.querySelector(`[id="${id}"]`);
    const bools = ['mihomo_auto_switch', 'mihomo_prefer_low_latency', 'mihomo_auto_optimize'];
    const ints = [
      'mihomo_optimize_interval_min',
      'mihomo_probe_timeout_sec',
      'mihomo_max_switches',
      'mihomo_cooldown_sec',
    ];
    const texts = ['mihomo_controller', 'mihomo_group', 'mihomo_proxy_url', 'mihomo_nodes'];
    const reasons = {
      optimization_running: '已有检测正在进行',
      switch_in_progress: '正在执行故障切换',
      switch_cooldown: '切换冷却中',
      invalid_configuration: '请先保存有效配置',
      controller_unavailable: '控制器连接失败',
      invalid_group: '策略组不是 Selector',
      latency_test_failed: '测速失败',
      no_eligible_node: '没有可用候选节点',
      selection_failed: '节点选择失败',
      anonymous_check_failed: '匿名验证失败，已保留或恢复原节点',
      restore_failed: '恢复原节点失败，请检查控制器',
    };
    const probeErrors = {
      'upstream Gemini region is unsupported': 'Gemini 不支持该地区',
      'anonymous probe network or timeout error': '网络失败或请求超时',
      'anonymous probe response is incomplete': '响应不完整',
      'anonymous probe has no generated reply': '没有返回回复正文',
      'anonymous probe was rejected upstream': '上游拒绝了测试请求',
    };
    function outcome(r) {
      return r.ok
        ? '验证成功'
        : (reasons[r.reason] || r.reason || '未完成') +
            (r.probe_error ? ' · ' + (probeErrors[r.probe_error] || r.probe_error) : '');
    }
    let actionRunning = false,
      serverRunning = false,
      panelSecretSet = false;
    async function api(path, options = {}) {
      const res = await fetch('api/' + path, { credentials: 'same-origin', ...options });
      if (res.status === 401) {
        unmount();
        currentPage = null;
        renderLogin();
        throw new Error('请登录管理员账号');
      }
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
      return data;
    }
    const json = (method, body) => ({
      method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    function rows(id, values) {
      const tbody = $(id);
      tbody.replaceChildren();
      for (const valuesRow of values) {
        const tr = document.createElement('tr');
        for (const value of valuesRow) {
          const td = document.createElement('td');
          td.textContent = String(value ?? '');
          tr.append(td);
        }
        tbody.append(tr);
      }
    }
    function option(name, label, selected = false, disabled = false) {
      const o = document.createElement('option');
      o.value = name;
      o.textContent = label;
      o.selected = selected;
      o.disabled = disabled;
      return o;
    }
    function renderStatus(data) {
      serverRunning = !!data.running;
      const last = data.history?.at(-1);
      $('state').textContent = [
        data.running
          ? '检测进行中'
          : data.config.mihomo_auto_optimize
            ? '自动检测已开启'
            : '自动检测已关闭',
        data.next_run ? '下次检测：' + new Date(data.next_run * 1000).toLocaleString() : '',
        last ? '最近节点：' + (last.node || '未选择') : '',
      ]
        .filter(Boolean)
        .join(' · ');
      $('run').disabled = serverRunning || actionRunning;
      rows(
        'history',
        (data.history || [])
          .slice()
          .reverse()
          .map((r) => [
            new Date(r.checked_at * 1000).toLocaleString(),
            outcome(r),
            r.node || '—',
            r.ok ? `${r.delay_ms} ms / ${r.probe_ms} ms` : '—',
          ]),
      );
      if (!data.history?.length) rows('history', [['尚无检测记录', '', '', '']]);
    }
    async function load(fill = false) {
      const data = await api('mihomo/config');
      if (!active) return data;
      panelSecretSet = !!data.secret_from_panel;
      if (fill) {
        for (const key of bools) $(key).checked = !!data.config[key];
        for (const key of ints)
          $(key).value =
            data.config[key] ??
            {
              mihomo_optimize_interval_min: 5,
              mihomo_probe_timeout_sec: 20,
              mihomo_max_switches: 2,
              mihomo_cooldown_sec: 5,
            }[key];
        for (const key of texts.filter((k) => k !== 'mihomo_group'))
          $(key).value = data.config[key] || '';
        const group = data.config.mihomo_group || '';
        $('mihomo_group').replaceChildren(option(group, group || '连接后选择策略组', true));
        $('clearSecret').disabled = !data.secret_from_panel;
        $('controllerSecret').placeholder = data.secret_set ? '已设置，留空继续使用' : '未设置';
        $('secretState').textContent = data.secret_set ? '密码已保存，不回显。' : '密码尚未设置。';
      }
      renderStatus(data);
      if (fill) await readLatency(false);
      return data;
    }
    async function busy(id, fn) {
      const button = $(id);
      button.disabled = true;
      try {
        await fn();
      } finally {
        button.disabled =
          (id === 'run' && serverRunning) || (id === 'clearSecret' && !panelSecretSet);
      }
    }
    $('connect').onclick = () =>
      busy('connect', async () => {
        try {
          const body = { controller: $('mihomo_controller').value.trim() },
            password = $('controllerSecret').value;
          if (password) body.secret = password;
          const data = await api('mihomo/groups', json('POST', body));
          if (
            body.controller !== $('mihomo_controller').value.trim() ||
            password !== $('controllerSecret').value
          ) {
            $('connectionMsg').textContent = '连接信息已变更，请重新连接';
            return;
          }
          const current = $('mihomo_group').value;
          $('mihomo_group').replaceChildren(option('', '请选择策略组'));
          for (const g of data.groups || [])
            $('mihomo_group').append(
              option(g.name, `${g.name} · ${g.type}`, g.name === current, !g.selectable),
            );
          if (current && !(data.groups || []).some((g) => g.name === current))
            $('mihomo_group').append(option(current, current + '（未找到）', true, true));
          $('connectionMsg').textContent = `已连接，${data.groups.length} 个策略组`;
        } catch (e) {
          $('connectionMsg').textContent = e.message;
        }
      });
    $('saveSecret').onclick = () =>
      busy('saveSecret', async () => {
        try {
          if (!$('controllerSecret').value) throw new Error('请输入要保存的密码');
          await api('mihomo/secret', json('PUT', { secret: $('controllerSecret').value }));
          $('controllerSecret').value = '';
          $('controllerSecret').placeholder = '已设置，留空继续使用';
          $('secretState').textContent = '密码已保存，不回显。';
          panelSecretSet = true;
          $('clearSecret').disabled = false;
          $('connectionMsg').textContent = '密码已保存';
        } catch (e) {
          $('connectionMsg').textContent = e.message;
        }
      });
    $('save').onclick = () =>
      busy('save', async () => {
        try {
          const body = {};
          for (const key of bools) body[key] = $(key).checked;
          for (const key of ints) {
            if (!$(key).checkValidity()) throw new Error('请检查数值范围');
            body[key] = Number($(key).value);
          }
          for (const key of texts) body[key] = $(key).value.trim();
          const data = await api('mihomo/config', json('PUT', body));
          $('saveMsg').textContent = '已保存并生效';
          renderStatus(data);
        } catch (e) {
          $('saveMsg').textContent = e.message;
        }
      });
    $('run').onclick = () =>
      busy('run', async () => {
        actionRunning = true;
        $('saveMsg').textContent = '正在读取已有延迟并匿名验证…';
        try {
          const r = await api('mihomo/optimize', { method: 'POST' });
          $('saveMsg').textContent = r.ok ? `验证成功：${r.node}，${r.delay_ms} ms` : outcome(r);
          await load();
          await readLatency(false);
        } catch (e) {
          $('saveMsg').textContent = e.message;
        } finally {
          actionRunning = false;
        }
      });
    async function readLatency(refresh) {
      $('latencyMsg').textContent = refresh ? ' 正在重新测速…' : ' 正在读取已有结果…';
      try {
        const r = await api('mihomo/latency', { method: refresh ? 'POST' : 'GET' });
        if (!active) return;
        rows(
          'nodes',
          r.nodes.map((n) => [
            n.name,
            n.delay_ms > 0 ? n.delay_ms + ' ms' : '无有效结果',
            n.current ? '当前节点' : '',
          ]),
        );
        $('latencyMsg').textContent = refresh
          ? ' 测速完成；节点没有切换'
          : ' 已读取 mihomo 已有结果；未发起测速';
      } catch (e) {
        $('latencyMsg').textContent = ' ' + e.message;
      }
    }
    $('latency').onclick = () => busy('latency', () => readLatency(true));
    $('readCached').onclick = () => busy('readCached', () => readLatency(false));

    $('clearSecret').onclick = () =>
      busy('clearSecret', async () => {
        const confirmed = await modalConfirm({
          title: '清除面板密码',
          desc: '清除后使用环境变量 MIHOMO_SECRET 或启动配置中的密码。',
          confirmText: '清除',
        });
        if (!confirmed || !active) return;
        try {
          const data = await api('mihomo/secret', json('PUT', { secret: '' }));
          $('controllerSecret').value = '';
          $('controllerSecret').placeholder = data.configured ? '已设置，留空继续使用' : '未设置';
          $('secretState').textContent = data.configured
            ? '使用部署配置中的密码。'
            : '密码尚未设置。';
          panelSecretSet = false;
          $('clearSecret').disabled = true;
          $('connectionMsg').textContent = '已清除面板密码';
        } catch (e) {
          $('connectionMsg').textContent = e.message;
        }
      });
    const timer = setInterval(() => {
      if (active && !document.hidden) load().catch(() => {});
    }, 3000);
    dispose = () => {
      active = false;
      clearInterval(timer);
    };
    await load(true).catch((e) => {
      if (active) $('state').textContent = e.message;
    });
  }
  return { render, mount, unmount };
})();
