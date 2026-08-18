#include <Arduino.h>
#include <RotaryEncoder.h>
#include <Wire.h>

// --- System Configuration ---
const unsigned long DEBOUNCE_DELAY = 50;
const int MAX_ENCODER_VALUE = 50;
const unsigned long DEEJ_SEND_INTERVAL = 15;
float globalBrightness = 0.15f; // Dynamic global brightness (0.0 to 1.0)

// Max Brightness Blink state
bool isMaxBlinking = false;
unsigned long maxBlinkEndTime = 0;

// --- LED Hardware & Color Definitions ---
const int MUX_SELECT_PIN = 10;
const int I2C_SDA_PIN = 12;
const int I2C_SCL_PIN = 11;

const int LEDS_PER_CHIP = 12;
const byte LED_CHIP_ADDRESSES[] = {0x30, 0x31, 0x32, 0x33};
const int NUM_CHIPS_PER_BANK =
    sizeof(LED_CHIP_ADDRESSES) / sizeof(LED_CHIP_ADDRESSES[0]);
const int LEDS_PER_BANK = NUM_CHIPS_PER_BANK * LEDS_PER_CHIP;
const int TOTAL_LEDS = 96;
const int ENCODER_LED_COUNT = 10;

const int ENCODER_LED_ORDER_E1[ENCODER_LED_COUNT] = {10, 8, 6, 4, 1,
                                                     2,  3, 5, 7, 9};
const int ENCODER_LED_ORDER_E2[ENCODER_LED_COUNT] = {1, 2, 10, 8, 6,
                                                     4, 3, 5,  7, 9};
const int ENCODER_LED_ORDER_E3[ENCODER_LED_COUNT] = {1, 2, 3, 4, 8,
                                                     5, 6, 7, 9, 10};
const int ENCODER_LED_ORDER_E4[ENCODER_LED_COUNT] = {1, 2, 3, 4, 5,
                                                     6, 7, 8, 9, 10};
const int ENCODER_LED_ORDER_E5[ENCODER_LED_COUNT] = {2, 4, 6, 7, 8,
                                                     5, 3, 1, 9, 10};
const int ENCODER_LED_ORDER_E6[ENCODER_LED_COUNT] = {2,  4, 6, 8, 9,
                                                     10, 7, 5, 3, 1};

const byte DEVICE_CONFIG0 = 0x00;
const byte OUT0_COLOR_ADDR = 0x14;

struct Color {
  byte r, g, b;
  bool operator!=(const Color &other) const {
    return r != other.r || g != other.g || b != other.b;
  }
};

const Color COLOR_WHITE = {255, 255, 255};
const Color COLOR_RED = {255, 0, 0};
const Color COLOR_OFF = {0, 0, 0};

// LED Buffers for smooth rendering
Color ledBuffer[TOTAL_LEDS + 1];
Color ledHardwareState[TOTAL_LEDS + 1]; // Tracks exact bytes pushed to driver chips

// Background State
Color backgroundColor = {0, 0, 0};
bool isRainbowMode = false;
uint8_t rainbowHueOffset = 0;
unsigned long lastRainbowUpdate = 0;

// --- Input Device Structs ---
struct EncoderInfo {
  const char *name;
  uint8_t btn_pin, rotA_pin, rotB_pin;
  int startLed;
  const int *ledOrder;
  uint8_t ledOrderLength;
  RotaryEncoder *encoder;
  long lastDetentPosition;
  bool isMuted;
  uint8_t lastButtonState;
  unsigned long lastDebounceTime;

  EncoderInfo(const char *n, uint8_t b, uint8_t ra, uint8_t rb, int sLed,
              const int *order, uint8_t orderLen)
      : name(n), btn_pin(b), rotA_pin(ra), rotB_pin(rb), startLed(sLed),
        ledOrder(order), ledOrderLength(orderLen) {
    lastDetentPosition = 0;
    isMuted = false;
    lastButtonState = HIGH;
    lastDebounceTime = 0;
    encoder = nullptr;
  }
};

struct ButtonInfo {
  const char *name;
  uint8_t pin;
  int ledNum;
  uint8_t lastState;
  unsigned long lastDebounceTime;
  unsigned long pressStartTime;
  unsigned long lastRepeatTime;
  bool isPressed;

  ButtonInfo(const char *n, uint8_t p, int ln) : name(n), pin(p), ledNum(ln) {
    lastState = HIGH;
    lastDebounceTime = 0;
    pressStartTime = 0;
    lastRepeatTime = 0;
    isPressed = false;
  }
};

EncoderInfo encoders[] = {
    EncoderInfo("E1", 1, 42, 2, 1, ENCODER_LED_ORDER_E1, ENCODER_LED_COUNT),
    EncoderInfo("E2", 41, 39, 40, 11, ENCODER_LED_ORDER_E2, ENCODER_LED_COUNT),
    EncoderInfo("E3", 38, 47, 48, 21, ENCODER_LED_ORDER_E3, ENCODER_LED_COUNT),
    EncoderInfo("E4", 21, 13, 14, 31, ENCODER_LED_ORDER_E4, ENCODER_LED_COUNT),
    EncoderInfo("E5", 9, 18, 8, 41, ENCODER_LED_ORDER_E5, ENCODER_LED_COUNT),
    EncoderInfo("E6", 17, 15, 16, 51, ENCODER_LED_ORDER_E6, ENCODER_LED_COUNT)};
const int numEncoders = sizeof(encoders) / sizeof(EncoderInfo);

// Buttons: Rol = Brightness+, Rul = Page Left, Ror = Brightness-, Rur = Page Right
ButtonInfo buttons[] = {ButtonInfo("Rol", 7, 61), ButtonInfo("Rul", 5, 64),
                        ButtonInfo("Ror", 6, 62), ButtonInfo("Rur", 4, 63)};
const int numButtons = sizeof(buttons) / sizeof(ButtonInfo);

// Page State (0 = Left / Rul, 1 = Right / Rur)
int currentPage = 0;
Color pageZeroColor[2][6];
Color pageFullColor[2][6];
Color pageButtonColor[2] = {COLOR_WHITE, COLOR_WHITE};
Color pageButtonOffColor[2] = {COLOR_OFF, COLOR_OFF};

unsigned long lastDeejSendTime = 0;

// --- Function Prototypes ---
void setSingleLedColor(int ledNum, const Color &c);
void renderLEDs();
void triggerMaxBlink();
void processDeejSerial();
Color parseHexColor(String hexStr);
Color hsvToRgb(uint8_t h, uint8_t s, uint8_t v);

// --- Interrupt Service Routine for Encoders ---
void IRAM_ATTR checkPosition() {
  for (int i = 0; i < numEncoders; i++) {
    encoders[i].encoder->tick();
  }
}

void triggerMaxBlink() {
  isMaxBlinking = true;
  maxBlinkEndTime = millis() + 450;
}

// --- Main Setup ---
void setup() {
  Serial.begin(9600);
  while (!Serial)
    ;

  Wire.begin(I2C_SDA_PIN, I2C_SCL_PIN);
  Wire.setClock(400000);

  pinMode(MUX_SELECT_PIN, OUTPUT);

  // Initialize default page colors
  for (int p = 0; p < 2; p++) {
    for (int e = 0; e < numEncoders; e++) {
      pageZeroColor[p][e] = COLOR_WHITE;
      pageFullColor[p][e] = COLOR_WHITE;
    }
  }

  // Initialize LED driver chips & clear buffers
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    ledBuffer[i] = COLOR_OFF;
    ledHardwareState[i] = {255, 255, 255}; // dummy initial state to force first write
  }

  for (int bank = 0; bank < 2; bank++) {
    digitalWrite(MUX_SELECT_PIN, bank == 0 ? LOW : HIGH);
    for (byte address : LED_CHIP_ADDRESSES) {
      Wire.beginTransmission(address);
      Wire.write(DEVICE_CONFIG0);
      Wire.write(0x40);
      Wire.endTransmission();
    }
  }
  digitalWrite(MUX_SELECT_PIN, LOW);
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    setSingleLedColor(i, COLOR_OFF);
  }

  // Setup Encoders with State Machine
  for (int i = 0; i < numEncoders; i++) {
    encoders[i].encoder =
        new RotaryEncoder(encoders[i].rotB_pin, encoders[i].rotA_pin,
                          RotaryEncoder::LatchMode::TWO03);
    pinMode(encoders[i].btn_pin, INPUT_PULLUP);

    attachInterrupt(digitalPinToInterrupt(encoders[i].rotA_pin), checkPosition,
                    CHANGE);
    attachInterrupt(digitalPinToInterrupt(encoders[i].rotB_pin), checkPosition,
                    CHANGE);
  }

  for (int i = 0; i < numButtons; i++) {
    pinMode(buttons[i].pin, INPUT_PULLUP);
  }

  renderLEDs();
}

// --- Main Loop ---
void loop() {
  bool needsRender = false;

  processDeejSerial();

  // Handle Max Brightness Blink Animation
  if (isMaxBlinking) {
    if (millis() < maxBlinkEndTime) {
      needsRender = true;
    } else {
      isMaxBlinking = false;
      needsRender = true;
    }
  }

  // Rainbow Animation Timer (~30 fps)
  if (isRainbowMode && globalBrightness > 0.01f && millis() - lastRainbowUpdate > 33) {
    rainbowHueOffset++;
    lastRainbowUpdate = millis();
    needsRender = true;
  }

  // Check Rotary Encoders
  for (int i = 0; i < numEncoders; i++) {
    long currentDetentPosition = encoders[i].encoder->getPosition();

    if (currentDetentPosition != encoders[i].lastDetentPosition) {
      if (currentDetentPosition > MAX_ENCODER_VALUE) {
        currentDetentPosition = MAX_ENCODER_VALUE;
        encoders[i].encoder->setPosition(MAX_ENCODER_VALUE);
      } else if (currentDetentPosition < 0) {
        currentDetentPosition = 0;
        encoders[i].encoder->setPosition(0);
      }
      encoders[i].lastDetentPosition = currentDetentPosition;
      needsRender = true;
    }
  }

  // Check Encoder Buttons (Mute Toggles)
  for (int i = 0; i < numEncoders; i++) {
    int reading = digitalRead(encoders[i].btn_pin);
    if (reading != encoders[i].lastButtonState &&
        millis() - encoders[i].lastDebounceTime > DEBOUNCE_DELAY) {
      encoders[i].lastDebounceTime = millis();
      encoders[i].lastButtonState = reading;
      if (reading == LOW) {
        encoders[i].isMuted = !encoders[i].isMuted;
        Serial.printf("M:%d\n", i);
        needsRender = true;
      }
    }
  }

  // Check Rubber Dome Buttons (Rol: Brightness+, Ror: Brightness-, Rul: Page Left, Rur: Page Right)
  for (int i = 0; i < numButtons; i++) {
    int reading = digitalRead(buttons[i].pin);
    if (reading != buttons[i].lastState &&
        millis() - buttons[i].lastDebounceTime > DEBOUNCE_DELAY) {
      buttons[i].lastDebounceTime = millis();
      buttons[i].lastState = reading;
      buttons[i].isPressed = (reading == LOW);
      needsRender = true;

      if (reading == LOW) {
        buttons[i].pressStartTime = millis();
        buttons[i].lastRepeatTime = millis();

        // Tap actions
        if (i == 0) { // Rol -> Brightness +
          if (globalBrightness >= 1.0f) {
            triggerMaxBlink();
          } else if (globalBrightness == 0.0f) {
            globalBrightness = 0.03f;
          } else {
            globalBrightness = min(1.0f, globalBrightness + 0.03f);
            if (globalBrightness >= 1.0f) {
              triggerMaxBlink();
            }
          }
        } else if (i == 2) { // Ror -> Brightness -
          if (globalBrightness > 0.01f) {
            globalBrightness = max(0.01f, globalBrightness - 0.03f);
          } else if (globalBrightness == 0.01f) {
            // Discrete press when already at 1% threshold turns off completely
            globalBrightness = 0.0f;
          }
        } else if (i == 1) { // Rul -> Page Left (Page 0)
          if (currentPage != 0) {
            currentPage = 0;
            Serial.println("P:0");
          }
        } else if (i == 3) { // Rur -> Page Right (Page 1)
          if (currentPage != 1) {
            currentPage = 1;
            Serial.println("P:1");
          }
        }
      }
    }

    // Continuous hold adjustment for Brightness (+ on Rol / - on Ror)
    if (buttons[i].isPressed && (i == 0 || i == 2)) {
      if (millis() - buttons[i].pressStartTime > 250 &&
          millis() - buttons[i].lastRepeatTime > 30) {
        buttons[i].lastRepeatTime = millis();
        if (i == 0) {
          if (globalBrightness < 1.0f) {
            globalBrightness = min(1.0f, globalBrightness + 0.02f);
            if (globalBrightness >= 1.0f) {
              triggerMaxBlink();
            }
            needsRender = true;
          }
        } else {
          // Hold on '-' stops at 0.01f (1% faders, 0% mood)
          if (globalBrightness > 0.01f) {
            globalBrightness = max(0.01f, globalBrightness - 0.02f);
            needsRender = true;
          }
        }
      }
    }
  }

  // Render LEDs if needed
  if (needsRender) {
    renderLEDs();
  }

  // Send continuous slider updates to Deej
  if (millis() - lastDeejSendTime >= DEEJ_SEND_INTERVAL) {
    lastDeejSendTime = millis();
    String payload = "";
    for (int i = 0; i < numEncoders; i++) {
      long mappedValue =
          map(encoders[i].lastDetentPosition, 0, MAX_ENCODER_VALUE, 0, 1023);
      payload += String(mappedValue);
      if (i < numEncoders - 1)
        payload += "|";
    }
    Serial.println(payload);
  }
}

// --- Deej Serial Parser ---
void processDeejSerial() {
  while (Serial.available()) {
    String line = Serial.readStringUntil('\n');
    line.trim();

    if (line.startsWith("V:")) {
      int c1 = line.indexOf(':');
      int c2 = line.indexOf(':', c1 + 1);
      if (c1 != -1 && c2 != -1) {
        int idx = line.substring(c1 + 1, c2).toInt();
        float percent = line.substring(c2 + 1).toFloat();
        if (idx >= 0 && idx < numEncoders) {
          long newPos = round(percent * MAX_ENCODER_VALUE);
          encoders[idx].lastDetentPosition = newPos;
          encoders[idx].encoder->setPosition(newPos);
          renderLEDs();
        }
      }
    } else if (line.startsWith("M:")) {
      int c1 = line.indexOf(':');
      int c2 = line.indexOf(':', c1 + 1);
      if (c1 != -1 && c2 != -1) {
        int idx = line.substring(c1 + 1, c2).toInt();
        int muted = line.substring(c2 + 1).toInt();
        if (idx >= 0 && idx < numEncoders) {
          encoders[idx].isMuted = (muted == 1);
          renderLEDs();
        }
      }
    } else if (line.startsWith("P:")) {
      String pageStr = line.substring(2);
      pageStr.trim();
      int p = (pageStr == "1" || pageStr.equalsIgnoreCase("right")) ? 1 : 0;
      if (currentPage != p) {
        currentPage = p;
        renderLEDs();
      }
    } else if (line.startsWith("CP:")) {
      int c1 = line.indexOf(':');
      int c2 = line.indexOf(':', c1 + 1);
      int c3 = line.indexOf(':', c2 + 1);
      if (c1 != -1 && c2 != -1) {
        String pStr = line.substring(c1 + 1, c2);
        int p = (pStr == "1" || pStr.equalsIgnoreCase("R") || pStr.equalsIgnoreCase("right")) ? 1 : 0;
        String activeHex = (c3 != -1) ? line.substring(c2 + 1, c3) : line.substring(c2 + 1);
        String offHex = (c3 != -1) ? line.substring(c3 + 1) : "#000000";
        pageButtonColor[p] = parseHexColor(activeHex);
        pageButtonOffColor[p] = parseHexColor(offHex);
        renderLEDs();
      }
    } else if (line.startsWith("C:")) {
      int c1 = line.indexOf(':');
      int c2 = line.indexOf(':', c1 + 1);
      int c3 = line.indexOf(':', c2 + 1);
      int c4 = line.indexOf(':', c3 + 1);

      if (c1 != -1 && c2 != -1 && c3 != -1 && c4 != -1) {
        String pStr = line.substring(c1 + 1, c2);
        int p = (pStr == "1" || pStr.equalsIgnoreCase("R") || pStr.equalsIgnoreCase("right")) ? 1 : 0;
        int idx = line.substring(c2 + 1, c3).toInt();
        if (idx >= 0 && idx < numEncoders) {
          pageZeroColor[p][idx] = parseHexColor(line.substring(c3 + 1, c4));
          pageFullColor[p][idx] = parseHexColor(line.substring(c4 + 1));
          renderLEDs();
        }
      } else if (c1 != -1 && c2 != -1 && c3 != -1) {
        int idx = line.substring(c1 + 1, c2).toInt();
        if (idx >= 0 && idx < numEncoders) {
          Color z = parseHexColor(line.substring(c2 + 1, c3));
          Color f = parseHexColor(line.substring(c3 + 1));
          pageZeroColor[0][idx] = z;
          pageFullColor[0][idx] = f;
          pageZeroColor[1][idx] = z;
          pageFullColor[1][idx] = f;
          renderLEDs();
        }
      }
    } else if (line.startsWith("BR:")) {
      String brStr = line.substring(3);
      brStr.trim();
      float val = brStr.toFloat();
      if (val > 1.0f) val = val / 100.0f;
      if (val < 0.0f) val = 0.0f;
      if (val > 1.0f) val = 1.0f;
      globalBrightness = val;
      renderLEDs();
    } else if (line.startsWith("B:")) {
      String hexStr = line.substring(2);
      hexStr.trim();
      if (hexStr.equalsIgnoreCase("rgb")) {
        isRainbowMode = true;
      } else {
        isRainbowMode = false;
        backgroundColor = parseHexColor(hexStr);
      }
      renderLEDs();
    }
  }
}

// --- Render Engine ---
void renderLEDs() {
  // 1. Draw Background (Solid or Rainbow) ONLY on Outer LEDs (65-96)
  // Background turns off completely when globalBrightness <= 0.01f
  bool bgAllowed = (globalBrightness > 0.01f);
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    if (i >= 65 && i <= 96) {
      if (bgAllowed) {
        if (isRainbowMode) {
          uint8_t pixelHue = rainbowHueOffset + ((i - 65) * 255 / 32);
          ledBuffer[i] = hsvToRgb(pixelHue, 255, 255);
        } else {
          ledBuffer[i] = backgroundColor;
        }
      } else {
        ledBuffer[i] = COLOR_OFF;
      }
    } else {
      ledBuffer[i] = COLOR_OFF;
    }
  }

  // 2. Overlay Encoders (Volume Lit LEDs for current page)
  for (int e = 0; e < numEncoders; e++) {
    EncoderInfo &enc = encoders[e];
    uint8_t orderLength =
        (enc.ledOrderLength > 0) ? enc.ledOrderLength : ENCODER_LED_COUNT;

    float percent = (float)enc.lastDetentPosition / MAX_ENCODER_VALUE;

    Color zeroC = pageZeroColor[currentPage][e];
    Color fullC = pageFullColor[currentPage][e];

    Color activeColor;
    if (enc.isMuted) {
      activeColor = COLOR_RED;
    } else {
      activeColor.r = zeroC.r + (fullC.r - zeroC.r) * percent;
      activeColor.g = zeroC.g + (fullC.g - zeroC.g) * percent;
      activeColor.b = zeroC.b + (fullC.b - zeroC.b) * percent;
    }

    float ledFill = percent * orderLength;
    if (enc.isMuted && ledFill < 1.0) {
      ledFill = 1.0;
    }
    int fullLeds = (int)ledFill;
    float partialFraction = ledFill - fullLeds;

    for (int i = 0; i < orderLength; i++) {
      int localLedIndex = enc.ledOrder ? enc.ledOrder[i] : (i + 1);
      int globalLedNum = enc.startLed + localLedIndex - 1;

      if (i < fullLeds) {
        ledBuffer[globalLedNum] = activeColor;
      } else if (i == fullLeds && partialFraction > 0.01) {
        Color bg = ledBuffer[globalLedNum];
        Color partialColor;
        partialColor.r =
            (byte)(bg.r + ((int)activeColor.r - (int)bg.r) * partialFraction);
        partialColor.g =
            (byte)(bg.g + ((int)activeColor.g - (int)bg.g) * partialFraction);
        partialColor.b =
            (byte)(bg.b + ((int)activeColor.b - (int)bg.b) * partialFraction);

        ledBuffer[globalLedNum] = partialColor;
      }
    }
  }

  // 3. Overlay Dome Buttons
  // LED 61 (Rol / Brightness+):
  // When max brightness is reached/pressed, blinks fast to indicate max.
  // Otherwise always shines at the current fader brightness.
  if (isMaxBlinking) {
    bool blinkState = (((maxBlinkEndTime - millis()) / 75) % 2 == 0);
    ledBuffer[61] = blinkState ? COLOR_WHITE : COLOR_OFF;
  } else {
    ledBuffer[61] = (globalBrightness > 0.0f) ? COLOR_WHITE : COLOR_OFF;
  }

  // LED 62 (Ror / Brightness-):
  // Always shines white at 1% brightness as long as fader brightness > 0%.
  // If fader brightness is 0%, LED 62 is also completely off (0%).
  ledBuffer[62] = (globalBrightness > 0.0f) ? COLOR_WHITE : COLOR_OFF;

  // LED 64 (Rul / Page Left): active color if page 0, else offcolor (scaled with faders)
  ledBuffer[64] = (currentPage == 0) ? pageButtonColor[0] : pageButtonOffColor[0];

  // LED 63 (Rur / Page Right): active color if page 1, else offcolor (scaled with faders)
  ledBuffer[63] = (currentPage == 1) ? pageButtonColor[1] : pageButtonOffColor[1];

  // 4. Push to Hardware (setSingleLedColor checks per-LED change against ledHardwareState)
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    setSingleLedColor(i, ledBuffer[i]);
  }
}

// --- Utilities ---
Color parseHexColor(String hexStr) {
  hexStr.trim();
  if (hexStr.startsWith("#"))
    hexStr = hexStr.substring(1);
  long number = strtol(hexStr.c_str(), nullptr, 16);
  Color c;
  c.r = (byte)((number >> 16) & 0xFF);
  c.g = (byte)((number >> 8) & 0xFF);
  c.b = (byte)(number & 0xFF);
  return c;
}

Color hsvToRgb(uint8_t h, uint8_t s, uint8_t v) {
  uint8_t r, g, b;
  uint8_t region, remainder, p, q, t;
  if (s == 0) {
    r = v;
    g = v;
    b = v;
  } else {
    region = h / 43;
    remainder = (h - (region * 43)) * 6;
    p = (v * (255 - s)) >> 8;
    q = (v * (255 - ((s * remainder) >> 8))) >> 8;
    t = (v * (255 - ((s * (255 - remainder)) >> 8))) >> 8;
    switch (region) {
    case 0:
      r = v;
      g = t;
      b = p;
      break;
    case 1:
      r = q;
      g = v;
      b = p;
      break;
    case 2:
      r = p;
      g = v;
      b = t;
      break;
    case 3:
      r = p;
      g = q;
      b = v;
      break;
    case 4:
      r = t;
      g = p;
      b = v;
      break;
    default:
      r = v;
      g = p;
      b = q;
      break;
    }
  }
  return {r, g, b};
}

byte applyBrightness(byte val, float factor) {
  if (factor <= 0.0f || val == 0)
    return 0;
  float scaled = val * factor;
  if (scaled > 0.0f && scaled < 1.0f)
    return 1;
  return (byte)round(scaled);
}

void setSingleLedColor(int ledNum, const Color &c) {
  if (ledNum < 1 || ledNum > TOTAL_LEDS)
    return;

  float factor = 0.0f;
  if (ledNum >= 1 && ledNum <= 60) {
    // Encoders / Faders: scale with globalBrightness
    factor = globalBrightness;
  } else if (ledNum == 61) {
    // '+' button: full during blink, otherwise globalBrightness (same as faders)
    factor = isMaxBlinking ? 1.0f : globalBrightness;
  } else if (ledNum == 62) {
    // '-' button: 1% as long as globalBrightness > 0, otherwise 0%
    factor = (globalBrightness > 0.0f) ? 0.01f : 0.0f;
  } else if (ledNum == 63 || ledNum == 64) {
    // Page switcher buttons: scale with globalBrightness (same as faders)
    factor = globalBrightness;
  } else if (ledNum >= 65 && ledNum <= 96) {
    // Mood / Surround: off when <= 1%, otherwise globalBrightness
    factor = (globalBrightness > 0.01f) ? globalBrightness : 0.0f;
  }

  byte outR = applyBrightness(c.r, factor);
  byte outG = applyBrightness(c.g, factor);
  byte outB = applyBrightness(c.b, factor);

  // Only transmit over I2C if the actual physical RGB output changed
  if (outR != ledHardwareState[ledNum].r ||
      outG != ledHardwareState[ledNum].g ||
      outB != ledHardwareState[ledNum].b) {

    int bankIndex = (ledNum - 1) / LEDS_PER_BANK;
    digitalWrite(MUX_SELECT_PIN, bankIndex == 0 ? LOW : HIGH);

    int ledNumInBank = (ledNum - 1) % LEDS_PER_BANK;
    int chipIndexInBank = ledNumInBank / LEDS_PER_CHIP;
    byte chipAddress = LED_CHIP_ADDRESSES[chipIndexInBank];
    int ledNumOnChip = ledNumInBank % LEDS_PER_CHIP;
    int baseOutput = ledNumOnChip * 3;

    Wire.beginTransmission(chipAddress);
    Wire.write(OUT0_COLOR_ADDR + baseOutput);
    Wire.write(outR);
    Wire.write(outG);
    Wire.write(outB);
    Wire.endTransmission();

    ledHardwareState[ledNum] = {outR, outG, outB};
  }
}