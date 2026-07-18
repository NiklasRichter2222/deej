package deej

const configUIHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>deej configuration UI</title>
  <style>
    :root { color-scheme: dark; }
    body { font-family: Arial, sans-serif; margin: 0; background: #1f1f1f; color: #f1f1f1; }
    .container { max-width: 1100px; margin: 0 auto; padding: 20px; }
    h1, h2 { margin: 0 0 12px; }
    section { background: #2a2a2a; border: 1px solid #3a3a3a; border-radius: 8px; padding: 14px; margin-bottom: 16px; }
    .row { display: flex; flex-wrap: wrap; gap: 12px; margin-bottom: 10px; }
    .col { flex: 1 1 220px; min-width: 220px; }
    label { font-size: 13px; display: block; margin-bottom: 6px; }
    input, select, button { width: 100%; box-sizing: border-box; padding: 8px; border-radius: 6px; border: 1px solid #555; background: #111; color: #f1f1f1; }
    button { cursor: pointer; background: #2f6feb; border-color: #2f6feb; }
    button.secondary { background: #333; border-color: #555; }
    .inline { display: flex; gap: 8px; align-items: center; }
    .inline input, .inline select, .inline button { flex: 1; }
    .toggle { display: flex; align-items: center; gap: 8px; }
    .toggle input { width: auto; }
    .slider-card { border: 1px solid #444; border-radius: 8px; padding: 10px; margin-bottom: 10px; }
    .chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px; }
    .chip { background: #0d1117; border: 1px solid #444; border-radius: 999px; padding: 4px 8px; font-size: 12px; }
    .chip button { width: auto; padding: 0 6px; margin-left: 4px; background: #444; border-color: #666; }
    .actions { display: flex; gap: 10px; }
    .actions button { flex: 1; }
    .muted { color: #9aa0a6; font-size: 12px; }
    #status { margin-bottom: 12px; font-size: 13px; color: #9ecbff; }
  </style>
</head>
<body>
  <div class="container">
    <h1>deej configuration</h1>
    <div id="status">Loading...</div>

    <section>
      <h2>Profiles</h2>
      <div class="row">
        <div class="col">
          <label>Saved profiles</label>
          <div class="inline">
            <select id="profiles"></select>
            <button class="secondary" id="loadProfile">Load profile</button>
          </div>
          <div class="muted">Loading copies the selected profile into config.yaml.</div>
        </div>
        <div class="col">
          <label>Save current as profile</label>
          <div class="inline">
            <input id="newProfileName" placeholder="e.g. gaming">
            <button class="secondary" id="saveProfile">Save profile</button>
          </div>
        </div>
      </div>
    </section>

    <section>
      <h2>Connection</h2>
      <div class="row">
        <div class="col">
          <label>Detected serial ports</label>
          <div class="inline">
            <select id="portSelect"></select>
            <button class="secondary" id="usePort">Use</button>
          </div>
        </div>
        <div class="col">
          <label>Manual COM/TTY port</label>
          <input id="comPort" placeholder="COM4 or /dev/ttyACM0">
        </div>
      </div>
      <div class="row">
        <div class="col">
          <label>Baud rate presets</label>
          <div class="inline">
            <select id="baudSelect"></select>
            <button class="secondary" id="useBaud">Use</button>
          </div>
        </div>
        <div class="col">
          <label>Manual baud rate</label>
          <input id="baudRate" type="number" min="1">
        </div>
      </div>
    </section>

    <section>
      <h2>Options</h2>
      <div class="row">
        <div class="col toggle"><input id="invertSliders" type="checkbox"><label for="invertSliders">Invert sliders</label></div>
        <div class="col toggle"><input id="sendOnStartup" type="checkbox"><label for="sendOnStartup">Send on startup</label></div>
        <div class="col toggle"><input id="syncVolumes" type="checkbox"><label for="syncVolumes">Sync volumes continuously</label></div>
      </div>
    </section>

    <section>
      <h2>Background lighting</h2>
      <div class="row">
        <div class="col">
          <label>Preset</label>
          <select id="bgPreset"></select>
        </div>
        <div class="col" id="bgCustomWrap" style="display:none;">
          <label>Custom color</label>
          <input id="bgCustom" type="color" value="#0000ff">
        </div>
      </div>
    </section>

    <section>
      <h2>Slider mapping</h2>
      <div class="row">
        <div class="col">
          <label>Number of sliders</label>
          <input id="sliderCount" type="number" min="1" max="32">
        </div>
      </div>
      <div id="sliders"></div>
    </section>

    <section>
      <h2>Slider LED colors</h2>
      <div id="colors"></div>
    </section>

    <div class="actions">
      <button id="reload">Reload from deej</button>
      <button id="save">Save to config.yaml</button>
    </div>
  </div>

  <script>
    let state;
    let sliderTargets = {};
    let sliderColors = {};

    const byId = (id) => document.getElementById(id);
    const status = (msg, isError = false) => {
      const el = byId('status');
      el.textContent = msg;
      el.style.color = isError ? '#ff7b72' : '#9ecbff';
    };

    async function request(url, options = {}) {
      const res = await fetch(url, { headers: { 'content-type': 'application/json' }, ...options });
      if (!res.ok) {
        throw new Error(await res.text());
      }
      return res.json();
    }

    function initMappingData(config) {
      sliderTargets = {};
      sliderColors = {};
      for (let i = 0; i < config.sliderCount; i++) {
        const key = String(i);
        sliderTargets[key] = [...(config.sliderMapping[key] || [])];
        sliderColors[key] = config.colorMapping[key] || { mode: 'gradient', zero: '#ff0000', full: '#00ff00' };
      }
    }

    function renderProfiles() {
      const profiles = byId('profiles');
      profiles.innerHTML = '';
      state.profiles.forEach((name) => {
        const opt = document.createElement('option');
        opt.value = name;
        opt.textContent = name;
        profiles.appendChild(opt);
      });
    }

    function renderConnection() {
      const portSelect = byId('portSelect');
      portSelect.innerHTML = '';
      state.serialPorts.forEach((p) => {
        const opt = document.createElement('option');
        opt.value = p.port;
        opt.textContent = p.description ? p.port + ' - ' + p.description : p.port;
        portSelect.appendChild(opt);
      });

      const baudSelect = byId('baudSelect');
      baudSelect.innerHTML = '';
      state.baudRateOptions.forEach((baud) => {
        const opt = document.createElement('option');
        opt.value = String(baud);
        opt.textContent = String(baud);
        baudSelect.appendChild(opt);
      });
    }

    function renderBackground() {
      const bgPreset = byId('bgPreset');
      bgPreset.innerHTML = '';
      state.bgPresets.forEach((preset) => {
        const opt = document.createElement('option');
        opt.value = preset.value;
        opt.textContent = preset.name;
        bgPreset.appendChild(opt);
      });

      const current = (state.config.backgroundLighting || '').toLowerCase();
      const match = state.bgPresets.find((x) => x.value.toLowerCase() === current);
      if (match) {
        bgPreset.value = match.value;
      } else if (current.startsWith('#') && current.length === 7) {
        bgPreset.value = 'custom';
        byId('bgCustom').value = current;
      } else {
        bgPreset.value = 'custom';
      }
      toggleBgCustom();
    }

    function renderSliders() {
      const count = Number(byId('sliderCount').value || 1);
      const sliders = byId('sliders');
      sliders.innerHTML = '';

      for (let i = 0; i < count; i++) {
        const key = String(i);
        if (!sliderTargets[key]) sliderTargets[key] = [];

        const card = document.createElement('div');
        card.className = 'slider-card';
        card.innerHTML = '<h3>Slider ' + i + '</h3>' +
          '<div class="inline"><select id="suggest-' + key + '"></select><button class="secondary" id="add-suggest-' + key + '">Add</button></div>' +
          '<div class="inline" style="margin-top:8px;"><input id="custom-' + key + '" placeholder="Custom target (e.g. game.exe, deej.current)"><button class="secondary" id="add-custom-' + key + '">Add custom</button></div>' +
          '<div class="chips" id="chips-' + key + '"></div>';
        sliders.appendChild(card);

        const suggest = byId('suggest-' + key);
        [...state.specialTargets, ...state.applications].forEach((target) => {
          const opt = document.createElement('option');
          opt.value = target;
          opt.textContent = target;
          suggest.appendChild(opt);
        });

        byId('add-suggest-' + key).onclick = () => {
          const target = suggest.value;
          if (target && !sliderTargets[key].includes(target)) sliderTargets[key].push(target);
          renderSliderChips(key);
        };

        byId('add-custom-' + key).onclick = () => {
          const input = byId('custom-' + key);
          const target = input.value.trim();
          if (target && !sliderTargets[key].includes(target)) sliderTargets[key].push(target);
          input.value = '';
          renderSliderChips(key);
        };

        renderSliderChips(key);
      }
    }

    function renderSliderChips(key) {
      const chips = byId('chips-' + key);
      if (!chips) return;
      chips.innerHTML = '';
      sliderTargets[key].forEach((target, idx) => {
        const chip = document.createElement('div');
        chip.className = 'chip';
        chip.textContent = target;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.textContent = '×';
        remove.onclick = () => {
          sliderTargets[key].splice(idx, 1);
          renderSliderChips(key);
        };
        chip.appendChild(remove);
        chips.appendChild(chip);
      });
    }

    function renderColors() {
      const count = Number(byId('sliderCount').value || 1);
      const wrap = byId('colors');
      wrap.innerHTML = '';

      for (let i = 0; i < count; i++) {
        const key = String(i);
        if (!sliderColors[key]) sliderColors[key] = { mode: 'gradient', zero: '#ff0000', full: '#00ff00' };
        const cfg = sliderColors[key];

        const card = document.createElement('div');
        card.className = 'slider-card';
        card.innerHTML = '<h3>Slider ' + i + '</h3>' +
          '<div class="row"><div class="col"><label>Color mode</label><select id="mode-' + key + '"><option value="single">Single color</option><option value="gradient">Color changes</option></select></div></div>' +
          '<div class="row"><div class="col"><label>Start color</label><input id="zero-' + key + '" type="color"></div><div class="col" id="full-wrap-' + key + '"><label>End color</label><input id="full-' + key + '" type="color"></div></div>';
        wrap.appendChild(card);

        byId('mode-' + key).value = cfg.mode || 'gradient';
        byId('zero-' + key).value = cfg.zero || '#ff0000';
        byId('full-' + key).value = cfg.full || '#00ff00';

        byId('mode-' + key).onchange = () => {
          sliderColors[key].mode = byId('mode-' + key).value;
          toggleColorMode(key);
        };
        byId('zero-' + key).onchange = () => { sliderColors[key].zero = byId('zero-' + key).value; };
        byId('full-' + key).onchange = () => { sliderColors[key].full = byId('full-' + key).value; };

        toggleColorMode(key);
      }
    }

    function toggleColorMode(key) {
      const mode = byId('mode-' + key).value;
      const fullWrap = byId('full-wrap-' + key);
      fullWrap.style.display = mode === 'single' ? 'none' : '';
      sliderColors[key].mode = mode;
    }

    function toggleBgCustom() {
      byId('bgCustomWrap').style.display = byId('bgPreset').value === 'custom' ? '' : 'none';
    }

    function collectConfig() {
      const sliderCount = Number(byId('sliderCount').value || 1);
      const sliderMapping = {};
      const colorMapping = {};

      for (let i = 0; i < sliderCount; i++) {
        const key = String(i);
        sliderMapping[key] = [...(sliderTargets[key] || [])];

        const color = sliderColors[key] || { mode: 'gradient', zero: '#ff0000', full: '#00ff00' };
        colorMapping[key] = {
          mode: color.mode || 'gradient',
          zero: color.zero || '#ff0000',
          full: color.full || '#00ff00',
        };
      }

      return {
        sliderCount,
        sliderMapping,
        comPort: byId('comPort').value.trim(),
        baudRate: Number(byId('baudRate').value || 9600),
        invertSliders: byId('invertSliders').checked,
        noiseReduction: state.config.noiseReduction || 'default',
        sendOnStartup: byId('sendOnStartup').checked,
        syncVolumes: byId('syncVolumes').checked,
        backgroundLighting: byId('bgPreset').value === 'custom' ? byId('bgCustom').value : byId('bgPreset').value,
        colorMapping,
        commands: state.config.commands,
      };
    }

    async function loadState() {
      status('Loading state...');
      state = await request('/api/state');

      byId('sliderCount').value = state.config.sliderCount;
      byId('comPort').value = state.config.comPort || '';
      byId('baudRate').value = state.config.baudRate || 9600;
      byId('invertSliders').checked = !!state.config.invertSliders;
      byId('sendOnStartup').checked = !!state.config.sendOnStartup;
      byId('syncVolumes').checked = !!state.config.syncVolumes;

      initMappingData(state.config);
      renderProfiles();
      renderConnection();
      renderBackground();
      renderSliders();
      renderColors();

      status('Ready');
    }

    async function saveConfig() {
      status('Saving config.yaml...');
      await request('/api/save', { method: 'POST', body: JSON.stringify({ config: collectConfig() }) });
      status('Saved config.yaml. deej should auto-reload it.');
      await loadState();
    }

    async function saveProfile() {
      const name = byId('newProfileName').value.trim();
      if (!name) {
        status('Please provide a profile name.', true);
        return;
      }

      status('Saving profile...');
      await request('/api/profiles/save', { method: 'POST', body: JSON.stringify({ name, config: collectConfig() }) });
      byId('newProfileName').value = '';
      await loadState();
      status('Profile saved.');
    }

    async function loadProfile() {
      const name = byId('profiles').value;
      if (!name) {
        status('No profile selected.', true);
        return;
      }

      status('Loading profile into config.yaml...');
      await request('/api/profiles/load', { method: 'POST', body: JSON.stringify({ name }) });
      await loadState();
      status('Profile loaded into config.yaml.');
    }

    byId('save').onclick = () => saveConfig().catch((err) => status(err.message, true));
    byId('reload').onclick = () => loadState().catch((err) => status(err.message, true));
    byId('saveProfile').onclick = () => saveProfile().catch((err) => status(err.message, true));
    byId('loadProfile').onclick = () => loadProfile().catch((err) => status(err.message, true));

    byId('usePort').onclick = () => {
      const selected = byId('portSelect').value;
      if (selected) byId('comPort').value = selected;
    };

    byId('useBaud').onclick = () => {
      const selected = byId('baudSelect').value;
      if (selected) byId('baudRate').value = selected;
    };

    byId('bgPreset').onchange = toggleBgCustom;

    byId('sliderCount').onchange = () => {
      const count = Number(byId('sliderCount').value || 1);
      byId('sliderCount').value = Math.max(1, Math.min(32, count));
      renderSliders();
      renderColors();
    };

    loadState().catch((err) => status(err.message, true));
  </script>
</body>
</html>
`
