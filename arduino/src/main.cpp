#include <Arduino.h>
#include <RotaryEncoder.h> // <-- New State Machine Encoder Library
#include <Wire.h>


// --- System Configuration ---
const unsigned long DEBOUNCE_DELAY = 50;
const int MAX_ENCODER_VALUE = 50;
const unsigned long DEEJ_SEND_INTERVAL = 15;
const float GLOBAL_BRIGHTNESS = 0.15; // 15% brightness limit

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

const Color COLOR_WHITE = {(byte)(255 * GLOBAL_BRIGHTNESS),
                           (byte)(255 * GLOBAL_BRIGHTNESS),
                           (byte)(255 * GLOBAL_BRIGHTNESS)};
const Color COLOR_RED = {(byte)(255 * GLOBAL_BRIGHTNESS), 0, 0};
const Color COLOR_OFF = {0, 0, 0};

// LED Buffers for smooth rendering
Color ledBuffer[TOTAL_LEDS + 1];
Color ledHardwareState[TOTAL_LEDS + 1];

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
  bool isPressed;
  bool isMuted;
  uint8_t lastButtonState;
  unsigned long lastDebounceTime;
  Color zeroColor;
  Color fullColor;

  EncoderInfo(const char *n, uint8_t b, uint8_t ra, uint8_t rb, int sLed,
              const int *order, uint8_t orderLen)
      : name(n), btn_pin(b), rotA_pin(ra), rotB_pin(rb), startLed(sLed),
        ledOrder(order), ledOrderLength(orderLen) {
    lastDetentPosition = 0;
    isPressed = false;
    isMuted = false;
    lastButtonState = HIGH;
    lastDebounceTime = 0;
    encoder = nullptr;
    zeroColor = COLOR_WHITE;
    fullColor = COLOR_WHITE;
  }
};

struct ButtonInfo {
  const char *name;
  uint8_t pin;
  int ledNum;
  uint8_t lastState;
  unsigned long lastDebounceTime;
  bool isPressed;

  ButtonInfo(const char *n, uint8_t p, int ln) : name(n), pin(p), ledNum(ln) {
    lastState = HIGH;
    lastDebounceTime = 0;
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

ButtonInfo buttons[] = {ButtonInfo("Rol", 7, 61), ButtonInfo("Rul", 5, 64),
                        ButtonInfo("Ror", 6, 62), ButtonInfo("Rur", 4, 63)};
const int numButtons = sizeof(buttons) / sizeof(ButtonInfo);

unsigned long lastDeejSendTime = 0;

// --- Function Prototypes ---
void setSingleLedColor(int ledNum, const Color &c);
void renderLEDs();
void processDeejSerial();
Color parseHexColor(String hexStr);
Color hsvToRgb(uint8_t h, uint8_t s, uint8_t v);

// --- Interrupt Service Routine for Encoders ---
void IRAM_ATTR checkPosition() {
  for (int i = 0; i < numEncoders; i++) {
    encoders[i].encoder->tick();
  }
}

// --- Main Setup ---
void setup() {
  Serial.begin(9600);
  while (!Serial)
    ;

  Wire.begin(I2C_SDA_PIN, I2C_SCL_PIN);
  Wire.setClock(400000); // Fast I2C to support smooth animations

  pinMode(MUX_SELECT_PIN, OUTPUT);

  // Initialize LED driver chips & clear buffers
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    ledBuffer[i] = COLOR_OFF;
    ledHardwareState[i] = COLOR_OFF;
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
    // SWAPPED rotB_pin and rotA_pin here to reverse the hardware spin direction
    encoders[i].encoder =
        new RotaryEncoder(encoders[i].rotB_pin, encoders[i].rotA_pin,
                          RotaryEncoder::LatchMode::TWO03);
    pinMode(encoders[i].btn_pin, INPUT_PULLUP);

    // Attach the common interrupt to both pins of every encoder
    attachInterrupt(digitalPinToInterrupt(encoders[i].rotA_pin), checkPosition,
                    CHANGE);
    attachInterrupt(digitalPinToInterrupt(encoders[i].rotB_pin), checkPosition,
                    CHANGE);
  }

  for (int i = 0; i < numButtons; i++) {
    pinMode(buttons[i].pin, INPUT_PULLUP);
  }
}

// --- Main Loop ---
void loop() {
  bool needsRender = false;

  processDeejSerial();

  // Rainbow Animation Timer (Updates ~30 times a second)
  if (isRainbowMode && millis() - lastRainbowUpdate > 33) {
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
        // Encoder knob clicked -> Send mute toggle command
        encoders[i].isMuted = !encoders[i].isMuted;
        Serial.printf("M:%d\n", i);
        needsRender = true;
      }
    }
  }

  // Check Rubber Dome Buttons (Rol-Rur -> O:7 to O:10)
  for (int i = 0; i < numButtons; i++) {
    int reading = digitalRead(buttons[i].pin);
    if (reading != buttons[i].lastState &&
        millis() - buttons[i].lastDebounceTime > DEBOUNCE_DELAY) {
      buttons[i].isPressed = (reading == LOW);
      buttons[i].lastDebounceTime = millis();
      buttons[i].lastState = reading;
      needsRender = true;
      if (buttons[i].isPressed)
        Serial.printf("O:%d\n", 7 + i);
    }
  }

  // Render LEDs if anything changed (or if rainbow is ticking)
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
    } else if (line.startsWith("C:")) {
      int c1 = line.indexOf(':');
      int c2 = line.indexOf(':', c1 + 1);
      int c3 = line.indexOf(':', c2 + 1);
      if (c1 != -1 && c2 != -1 && c3 != -1) {
        int idx = line.substring(c1 + 1, c2).toInt();
        if (idx >= 0 && idx < numEncoders) {
          encoders[idx].zeroColor = parseHexColor(line.substring(c2 + 1, c3));
          encoders[idx].fullColor = parseHexColor(line.substring(c3 + 1));
          renderLEDs();
        }
      }
    }
  }
}

// --- Render Engine ---
void renderLEDs() {
  // 1. Draw Background (Solid or Rainbow) ONLY on Outer LEDs (65-96)
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    if (i >= 65 && i <= 96) {
      if (isRainbowMode) {
        // Offset hue based on physical position.
        // We calculate the spread across just these 32 LEDs so the rainbow
        // wraps perfectly.
        uint8_t pixelHue = rainbowHueOffset + ((i - 65) * 255 / 32);
        ledBuffer[i] = hsvToRgb(pixelHue, 255, 255);
      } else {
        ledBuffer[i] = backgroundColor;
      }
    } else {
      // Keep Encoders (1-60) and Dome Buttons (61-64) background completely OFF
      ledBuffer[i] = COLOR_OFF;
    }
  }

  // 2. Overlay Encoders (Volume Lit LEDs)
  for (int e = 0; e < numEncoders; e++) {
    EncoderInfo &enc = encoders[e];
    uint8_t orderLength =
        (enc.ledOrderLength > 0) ? enc.ledOrderLength : ENCODER_LED_COUNT;

    // Calculate exactly how far along the volume is (0.0 to 1.0)
    float percent = (float)enc.lastDetentPosition / MAX_ENCODER_VALUE;

    Color activeColor;
    if (enc.isMuted) {
      activeColor = COLOR_RED;
    } else {
      activeColor.r =
          enc.zeroColor.r + (enc.fullColor.r - enc.zeroColor.r) * percent;
      activeColor.g =
          enc.zeroColor.g + (enc.fullColor.g - enc.zeroColor.g) * percent;
      activeColor.b =
          enc.zeroColor.b + (enc.fullColor.b - enc.zeroColor.b) * percent;
    }

    // Calculate how many "LEDs worth" of volume we have (e.g., 9.2)
    float ledFill = percent * orderLength;
    if (enc.isMuted && ledFill < 1.0) {
      // When muted and volume is 0 or very low, light up at least 1 red LED
      // so the user clearly sees that this channel is muted.
      ledFill = 1.0;
    }
    int fullLeds = (int)ledFill; // e.g., 9 fully lit LEDs
    float partialFraction =
        ledFill - fullLeds; // e.g., 0.2 brightness for the 10th LED

    for (int i = 0; i < orderLength; i++) {
      int localLedIndex = enc.ledOrder ? enc.ledOrder[i] : (i + 1);
      int globalLedNum = enc.startLed + localLedIndex - 1;

      if (i < fullLeds) {
        // LED is fully engulfed by the volume level
        ledBuffer[globalLedNum] = activeColor;
      } else if (i == fullLeds && partialFraction > 0.01) {
        // LED is partially engulfed. Crossfade it smoothly over the existing
        // background color.
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
      // If i > fullLeds, do nothing (leave the background color alone)
    }
  }

  // 3. Overlay Dome Buttons
  for (int i = 0; i < numButtons; i++) {
    if (buttons[i].isPressed) {
      ledBuffer[buttons[i].ledNum] = COLOR_WHITE;
    }
  }

  // 4. Push to Hardware (Only send if the color changed to save I2C time)
  for (int i = 1; i <= TOTAL_LEDS; i++) {
    if (ledBuffer[i] != ledHardwareState[i]) {
      setSingleLedColor(i, ledBuffer[i]);
      ledHardwareState[i] = ledBuffer[i];
    }
  }
}

// --- Utilities ---
Color parseHexColor(String hexStr) {
  hexStr.trim();
  if (hexStr.startsWith("#"))
    hexStr = hexStr.substring(1);
  long number = strtol(hexStr.c_str(), nullptr, 16);
  Color c;
  c.r = (byte)(((number >> 16) & 0xFF) * GLOBAL_BRIGHTNESS);
  c.g = (byte)(((number >> 8) & 0xFF) * GLOBAL_BRIGHTNESS);
  c.b = (byte)((number & 0xFF) * GLOBAL_BRIGHTNESS);
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
  return {(byte)(r * GLOBAL_BRIGHTNESS), (byte)(g * GLOBAL_BRIGHTNESS),
          (byte)(b * GLOBAL_BRIGHTNESS)};
}

void setSingleLedColor(int ledNum, const Color &c) {
  if (ledNum < 1 || ledNum > TOTAL_LEDS)
    return;

  int bankIndex = (ledNum - 1) / LEDS_PER_BANK;
  digitalWrite(MUX_SELECT_PIN, bankIndex == 0 ? LOW : HIGH);

  int ledNumInBank = (ledNum - 1) % LEDS_PER_BANK;
  int chipIndexInBank = ledNumInBank / LEDS_PER_CHIP;
  byte chipAddress = LED_CHIP_ADDRESSES[chipIndexInBank];
  int ledNumOnChip = ledNumInBank % LEDS_PER_CHIP;
  int baseOutput = ledNumOnChip * 3;

  Wire.beginTransmission(chipAddress);
  Wire.write(OUT0_COLOR_ADDR + baseOutput);
  Wire.write(c.r);
  Wire.write(c.g);
  Wire.write(c.b);
  Wire.endTransmission();
}