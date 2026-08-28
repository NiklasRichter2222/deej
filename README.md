# deej

deej is an **open-source hardware volume mixer** for Windows and Linux PCs. It lets you use physical rotary controls to **seamlessly control the volumes of different applications** (such as your music player, the game you're playing, and your voice chat session) without having to Alt-Tab or interrupt what you're doing.

This enhanced version repurposes the premium hardware of an **AVerMedia Live Streamer AX310** (e.g. units with dead mainboards) by replacing the internal electronics with a **custom ESP32-S3 mainboard**. The custom PCB features dedicated voltage regulation and an **integrated USB hub**—allowing an internal **Stream Deck module** to be daisy-chained and mounted directly where the original AX310 screen was. The controller provides **6 rotary encoders with push-to-mute**, a **dual-page system (12 virtual channels)**, **96 dynamic RGB LEDs** with per-channel color gradients and ambient backlighting, **dedicated hardware brightness controls**, **bidirectional PC sync**, and **resilient auto-reconnect logic**.

**Join the official [deej Discord server](https://discord.gg/nf88NJu) for community discussion, questions, and support!**

[![Discord](https://img.shields.io/discord/702940502038937667?logo=discord)](https://discord.gg/nf88NJu)

---

## Showcase

<!-- ================================================================= -->
<!-- PLACEHOLDER: Insert your final design photo(s) below             -->
<!-- Replace 'assets/final-build.jpg' with the path to your photo(s) -->
<!-- ================================================================= -->
![Deej Final Build](assets/final-build.jpg)
*Custom ESP32-S3 Deej Controller (AX310 Conversion) with 6 Rotary Encoders, 96-LED Lighting Matrix, and Internal Stream Deck Integration*

---

## Table of contents

- [User Guide](#user-guide)
  - [Features Overview](#features-overview)
  - [Controller Layout & Usage](#controller-layout--usage)
  - [How to Run (Quick Start)](#how-to-run-quick-start)
  - [Browser-based Configuration UI](#browser-based-configuration-ui)
  - [Slider Mapping & Configuration (`config.yaml`)](#slider-mapping--configuration-configyaml)
- [Developer & Builder Guide](#developer--builder-guide)
  - [Bill of Materials (BOM) & Donor Hardware](#bill-of-materials-bom--donor-hardware)
  - [Custom PCB & Hardware Architecture](#custom-pcb--hardware-architecture)
  - [Firmware Guide (PlatformIO)](#firmware-guide-platformio)
  - [Building Desktop Client from Source](#building-desktop-client-from-source)
  - [Serial Communication Protocol](#serial-communication-protocol)
- [Community & License](#community--license)

---

# User Guide

## Features Overview

- **6 Rotary Encoders with Tactile Detents**: Smooth volume control with precise tactile feedback.
- **Push-to-Mute on Every Knob**: Click any encoder to instantly mute or unmute its mapped application. The volume arc turns red when muted.
- **Dual-Page Switching (12 Channels Total)**: Dedicated hardware buttons switch between **Page 1 (Left)** and **Page 2 (Right)**, doubling the physical knobs into 12 distinct controllable channels.
- **Hardware Brightness Controls**: Adjust global LED brightness directly from the controller with tap, smooth continuous hold-to-ramp, and max-brightness blink indication.
- **96-LED Matrix Lighting**:
  - **Encoder Volume Arcs**: 10-LED ring per knob showing exact volume levels with smooth partial-LED interpolation.
  - **Color Gradients**: Configure custom start (0%) and full (100%) RGB colors per channel and per page.
  - **Ambient Mood Backlighting**: Peripheral surround lighting supporting dynamic RGB rainbow cycling or solid hex colors.
- **Resilient Auto-Reconnection**: Background serial supervisor recovers connection seamlessly on PC sleep/wake cycles or USB reconnections.
- **Bidirectional Volume Sync**: PC-side volume changes (via Windows mixer, keyboard shortcuts, or on-screen sliders) immediately mirror back to the controller's LED rings.
- **Built-in Configuration UI**: Accessible directly from the system tray menu to visually map sliders, choose colors, and manage profiles.
- **Lightweight**: Desktop client written in Go uses ~10 MB of RAM and runs quietly in the system tray.

---

## Controller Layout & Usage

```
+-----------------------------------------------------------------------+
|                            DEEJ CONTROLLER                            |
|                                                                       |
|             [ Rol ] Brightness +       [ Ror ] Brightness -           |
|             [ Rul ] Page Left (Page 0) [ Rur ] Page Right (Page 1)    |
|                                                                       |
|     ( E1 )       ( E2 )       ( E3 )       ( E4 )       ( E5 )       ( E6 )   |
|      Knob         Knob         Knob         Knob         Knob         Knob    |
+-----------------------------------------------------------------------+
```

### Controls Summary

| Control | Action | Function |
| :--- | :--- | :--- |
| **Knobs (`E1` – `E6`)** | **Rotate** | Increases or decreases volume for the mapped application on the active page. |
| **Knobs (`E1` – `E6`)** | **Press (Click)** | Toggles mute for that channel. The volume arc turns solid red while muted. |
| **`Rol` (Top Left)** | **Single Tap** | Increases LED brightness by 3%. Blinks quickly upon reaching 100% max brightness. |
| **`Rol` (Top Left)** | **Press & Hold** | Continuously ramps brightness up to 100%. |
| **`Ror` (Top Right)** | **Single Tap** | Decreases LED brightness by 3%. Tapping at 1% turns LEDs completely off. |
| **`Ror` (Top Right)** | **Press & Hold** | Continuously ramps brightness down to 1% (ambient backlighting turns off). |
| **`Rul` (Bottom Left)** | **Single Tap** | Switches active layer to **Page 1 (Left)**. |
| **`Rur` (Bottom Right)** | **Single Tap** | Switches active layer to **Page 2 (Right)**. |

---

## How to Run (Quick Start)

### Requirements
- **Windows**: Windows 10 or Windows 11 (64-bit).
- **Linux**: Supported with GTK/AppIndicator dependencies (`libgtk-3-dev`, `libappindicator3-dev`, `libwebkit2gtk-4.0-dev`).

### Running the Pre-built Client

1. Download or copy `deej.exe` (or `deej-release.exe`) and [`config.yaml`](./config.yaml) into the same folder.
2. Plug in your Deej controller via USB.
3. Open `config.yaml` to confirm your `com_port` (e.g. `COM8`), or configure it through the tray UI.
4. Run `deej.exe`. A Deej icon will appear in your system tray.
5. *(Optional)* To run on Windows startup, create a shortcut to `deej.exe` and place it in:
   ```
   %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup
   ```

---

## Browser-based Configuration UI

deej includes a built-in web configuration dashboard accessible from your system tray:

1. Right-click the **deej** tray icon in your system taskbar.
2. Click **Open configuration UI**.
3. In the web interface you can:
   - Select currently running applications from a live dropdown list.
   - Assign apps and devices across both **Page 1 (Left)** and **Page 2 (Right)**.
   - Configure custom zero% and full% gradient colors per channel with visual color pickers.
   - Adjust ambient lighting mode (Rainbow / Off / Solid color) and default brightness.
   - Save and load configuration profiles stored in `profiles/*.yaml`.
4. Click **Save to config.yaml** to apply changes instantly.

---

## Slider Mapping & Configuration (`config.yaml`)

deej uses a simple YAML configuration file named [`config.yaml`](./config.yaml).

> [!TIP]
> **Live Reloading**: Any edits saved to `config.yaml` take effect immediately without restarting deej.

### Example `config.yaml`

```yaml
# --- Slider Mapping ---
# Process names are case-insensitive.
# Supported special targets: master, mic, system, deej.current, deej.unmapped, or exact audio device names.
slider_count: 6
slider_mapping:
  left:
    0: master
    1: deej.current
    2: ts3client_win64.exe
    3: discord.exe
    4:
      - spotify.exe
      - youtube-music-desktop-app.exe
    5: mic
  right:
    0: master
    1: deej.current
    2: ts3client_win64.exe
    3: discord.exe
    4:
      - spotify.exe
      - youtube-music-desktop-app.exe
    5: mic

# --- General Options ---
default_brightness: 0.15
invert_sliders: false
noise_reduction: default

# --- Connection Settings ---
com_port: COM8
baud_rate: 9600

# --- Bidirectional Sync Settings ---
send_on_startup: true
sync_volumes: true

# --- Controller Lighting ---
background_lighting: rgb
color_mapping:
  left:
    color: '#ffffff'
    offcolor: '#ff0000'
    0:
      zero: '#00ffff'
      full: '#ff0000'
    1:
      zero: '#ff7b00'
      full: '#ff7b00'
    2:
      zero: '#00ffee'
      full: '#001eff'
    3:
      zero: '#5662f6'
      full: '#5662f6'
    4:
      zero: '#ff0033'
      full: '#ff0033'
    5:
      zero: '#ff0000'
      full: '#00ff00'
  right:
    color: '#ffffff'
    offcolor: '#00ff00'
    0:
      zero: '#00ffff'
      full: '#ff0000'
    1:
      zero: '#ff7b00'
      full: '#ff7b00'
    2:
      zero: '#00ffee'
      full: '#001eff'
    3:
      zero: '#5662f6'
      full: '#5662f6'
    4:
      zero: '#ff0033'
      full: '#ff0033'
    5:
      zero: '#ff0000'
      full: '#00ff00'
```

### Configuration Options Breakdown

| Setting | Type | Description |
| :--- | :--- | :--- |
| `slider_count` | `int` | Number of physical knobs per page (default: `6`). |
| `slider_mapping.left` | `map` | Mappings for **Page 1 (Left)** channels `0` to `5`. |
| `slider_mapping.right` | `map` | Mappings for **Page 2 (Right)** channels `0` to `5`. |
| `default_brightness` | `float` | Startup LED brightness level (`0.0` to `1.0`, e.g. `0.15` for 15%). |
| `invert_sliders` | `bool` | Set `true` to invert rotation/volume direction. |
| `noise_reduction` | `string` | Jitter filter strength: `low`, `default`, or `high`. |
| `com_port` | `string` | Serial port used by the controller (e.g. `COM8` on Windows, `/dev/ttyUSB0` on Linux). |
| `baud_rate` | `int` | Serial communication baud rate (default: `9600`). |
| `send_on_startup` | `bool` | Sends initial PC volume levels and lighting config to the controller on launch. |
| `sync_volumes` | `bool` | Continuously mirrors PC-side volume and mute changes back to the hardware. |
| `background_lighting` | `string` | Ambient mood backlighting: `rgb` (rainbow mode), `off`, or a `#hex` color (e.g. `#0000ff`). |
| `color_mapping.left` / `.right` | `map` | Active page button color (`color`), inactive color (`offcolor`), and channel LED start (`zero`) and full (`full`) hex colors. |

#### Special Mapping Targets
- `master`: System master playback audio volume.
- `mic`: Default microphone input recording volume.
- `deej.current`: Controls whichever window is currently in the active foreground.
- `deej.unmapped`: Catch-all for any running audio application not bound to another channel.
- `system`: Windows System Sounds mixer channel.
- `Audio Device Name`: Exact name of an output or input endpoint, e.g. `Speakers (Realtek Audio)`.
- `List of Process Names`: Bind multiple executables to a single slider (e.g. a list of games or media players).

---

# Developer & Builder Guide

## Bill of Materials (BOM) & Donor Hardware

This build is designed around repurposing a **dead/donor AVerMedia Live Streamer AX310 (NEXUS)** unit and replacing its proprietary mainboard with an open-source ESP32-S3 PCB.

| Component | Source / Qty | Description |
| :--- | :---: | :--- |
| **Donor Hardware (AX310)** | 1x AVerMedia AX310 | Donor unit providing the housing/enclosure, 6 rotary encoders with push switch, 4 rubber dome buttons (`Rol`, `Rul`, `Ror`, `Rur`), and 96-LED front matrix boards |
| **Custom Replacement Mainboard** | 1x Custom PCB | Drop-in replacement PCB (Gerber files in [`pcb/`](./pcb)) interfacing with AX310 sub-boards |
| **Microcontroller** | 1x ESP32-S3 | ESP32-S3 module / dev board (running native USB CDC Serial) |
| **Voltage Regulation** | 1x Circuit | Onboard buck/LDO regulation circuit supplying clean power to ESP32-S3, LED matrix, and internal peripherals |
| **USB Hub Controller** | 1x IC | Onboard USB 2.0 hub controller chip splitting upstream USB into internal lines |
| **Stream Deck Module** *(Optional Mod)* | 1x Module | Stream Deck module mounted internally in place of the original AX310 screen, connected via the internal USB hub header |
| **USB Cable** | 1x USB-C | Single USB Type-C cable for data & power to the PC |

---

## Custom PCB & Hardware Architecture

### PCB Design Files & Showcase

All design files, schematics, and manufacturing archives (Gerbers / KiCad project) are located in the [`pcb/`](./pcb) directory.

<!-- ================================================================= -->
<!-- PLACEHOLDER: Insert your PCB images below                         -->
<!-- Replace 'pcb/pcb-front.png' and 'pcb/pcb-back.png' with your files -->
<!-- ================================================================= -->

| PCB Top View | PCB Bottom View |
| :---: | :---: |
| ![PCB Front](pcb/pcb-front.png)<br>*(Top Layer / Component Placement)* | ![PCB Back](pcb/pcb-back.png)<br>*(Bottom Layer / Routing)* |

> [!NOTE]
> Detailed hardware schematics, BOM exports, and Gerber archives can be found inside the [`pcb/`](./pcb) folder.

### Hardware Architecture

- **Drop-in Mainboard Replacement**: Replaces the dead AX310 motherboard while interfacing directly with the original AX310 encoder daughterboard, rubber dome buttons, and LED matrix.
- **Integrated USB Hub**: An onboard USB 2.0 hub controller allows a single external USB-C connection to route data to both the ESP32-S3 and an internal USB port for daisy-chaining a Stream Deck module (mounted in place of the original AX310 screen).
- **Onboard Power Regulation**: Dedicated power delivery network ensuring stable voltage for the ESP32-S3, 96 RGB LEDs, and connected USB accessories.
- **Encoder Inputs**: 6 rotary encoders connected with hardware interrupts for responsive quadrature decoding and debounced push-button sensing.
- **I2C Bus & Multiplexing**:
  - `I2C_SDA_PIN`: GPIO 12
  - `I2C_SCL_PIN`: GPIO 11 (400 kHz bus clock)
  - `MUX_SELECT_PIN`: GPIO 10 (switches between Bank 0 and Bank 1)
- **96-LED Mapping Breakdown**:
  - **LEDs 1 – 60**: 6 encoder volume arcs (10 LEDs per knob, ordered according to encoder board routing).
  - **LED 61**: `Rol` (Brightness + button indicator).
  - **LED 62**: `Ror` (Brightness - button indicator).
  - **LED 63**: `Rur` (Page Right button indicator).
  - **LED 64**: `Rul` (Page Left button indicator).
  - **LEDs 65 – 96**: 32 Ambient / Mood surround backlighting LEDs.
- **Power Management**: Auto-sleep detection turns off all LEDs after 5 seconds of lost serial communication / PC sleep.

---

## Firmware Guide (PlatformIO)

The firmware is written in C++ for the ESP32-S3 using the Arduino framework and managed via [PlatformIO](https://platformio.org/).

- **Source Code**: [`arduino/src/main.cpp`](./arduino/src/main.cpp)
- **PlatformIO Config**: [`platformio.ini`](./platformio.ini)
- **Target Environment**: `[env:esp32-s3-devkitc-1]`

### Flashing the Firmware

1. Install [VS Code](https://code.visualstudio.com/) and the [PlatformIO IDE extension](https://marketplace.visualstudio.com/items?itemName=platformio.platformio-ide) (or PlatformIO CLI).
2. Open this repository in VS Code.
3. Connect your ESP32-S3 board via USB.
4. Build and upload:
   ```bash
   pio run --target upload
   ```
5. Open the serial monitor if needed:
   ```bash
   pio device monitor
   ```

---

## Building Desktop Client from Source

The desktop client is written in Go. To compile it from source:

### Prerequisites
- [Go](https://golang.org/dl/) (version 1.18 or newer).
- Windows SDK / Cgo tools (if building for Windows).

### Build Commands

1. Clone the repository:
   ```bash
   git clone https://github.com/NiklasRichter2222/deej.git
   cd deej
   ```
2. Build using the provided Windows batch scripts:
   ```cmd
   pkg\deej\scripts\windows\build-all.bat
   ```
   Or build directly using the Go CLI:
   ```bash
   go build -ldflags "-H windowsgui" -o deej.exe ./pkg/deej/cmd
   ```

---

## Serial Communication Protocol

The PC client and ESP32-S3 communicate over a 9600-baud serial link using lightweight string payloads.

### Controller to PC (Uplink)

- **Slider Stream**: Sends continuous pipe-separated values for all channels across both pages:
  ```
  <P0_E0>|<P0_E1>|<P0_E2>|<P0_E3>|<P0_E4>|<P0_E5>|<P1_E0>|<P1_E1>|<P1_E2>|<P1_E3>|<P1_E4>|<P1_E5>
  ```
  *(Values range from `0` to `1023`)*
- **Mute Toggle**: Sent on encoder push-button click:
  ```
  M:<channel_index>
  ```

### PC to Controller (Downlink)

- **Volume Sync**: `V:<channel_index>:<percent>` (e.g. `V:0:0.75`)
- **Mute Sync**: `M:<channel_index>:<0|1>` (e.g. `M:0:1` turns LED arc red)
- **Page Button Colors**: `CP:<page_index>:<active_hex>:<off_hex>` (e.g. `CP:0:#ffffff:#ff0000`)
- **Channel Colors**: `C:<channel_index>:<zero_hex>:<full_hex>` (e.g. `C:0:#00ffff:#ff0000`)
- **Global Brightness**: `BR:<value>` (e.g. `BR:0.25`)
- **Background Mode**: `B:<rgb|#hex>` (e.g. `B:rgb` or `B:#0000ff`)
- **Heartbeat**: `HB` / `PING`

---

# Community & License

- **Discord**: Join the [deej Discord server](https://discord.gg/nf88NJu) to share builds and ask questions.
- **Community Builds**: Browse community creations in [`community.md`](./community.md).
- **Contributing**: See [`docs/CONTRIBUTING.md`](./docs/CONTRIBUTING.md).
- **License**: Released under the [MIT License](./LICENSE).
