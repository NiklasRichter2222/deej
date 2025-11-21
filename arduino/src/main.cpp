#include <Arduino.h>
#include <Wire.h>
#include <InterruptEncoder.h>

// --- System Configuration ---
const unsigned long DEBOUNCE_DELAY = 50;
const int MAX_ENCODER_VALUE = 100; // Increased for more granular control (0-100%)
const float ENCODER_VOLUME_PER_COUNT = 0.5f; // Volume percent change per encoder detent (adjust for sensitivity)
// --- Serial Communication ---
const long SERIAL_BAUD_RATE = 9600;
String serialBuffer = "";

// --- LED Hardware & Color Definitions ---
struct Color { byte r, g, b; };
Color lerp(const Color& a, const Color& b, float t) {
  return {
    (byte)(a.r + (b.r - a.r) * t),
    (byte)(a.g + (b.g - a.g) * t),
    (byte)(a.b + (b.b - a.b) * t)
  };
}

const int LEDS_PER_CHIP = 12;
const byte LED_CHIP_ADDRESSES[] = {0x30, 0x31, 0x32, 0x33};
const int NUM_CHIPS_PER_BANK = sizeof(LED_CHIP_ADDRESSES) / sizeof(LED_CHIP_ADDRESSES[0]);
const int LEDS_PER_BANK = NUM_CHIPS_PER_BANK * LEDS_PER_CHIP;
const int TOTAL_LEDS = 96;
const int ENCODER_LED_COUNT = 10;
const int ENCODER_LED_ORDER_E1[ENCODER_LED_COUNT] = {10,8,6,4,1,2,3,5,7,9};
const int ENCODER_LED_ORDER_E2[ENCODER_LED_COUNT] = {1,2,10,8,6,4,3,5,7,9};
const int ENCODER_LED_ORDER_E3[ENCODER_LED_COUNT] = {1,2,3,4,8,5,6,7,9,10};
const int ENCODER_LED_ORDER_E4[ENCODER_LED_COUNT] = {1,2,3,4,5,6,7,8,9,10};
const int ENCODER_LED_ORDER_E5[ENCODER_LED_COUNT] = {2,4,6,7,8,5,3,1,9,10};
const int ENCODER_LED_ORDER_E6[ENCODER_LED_COUNT] = {2,4,6,8,9,10,7,5,3,1};

// --- Background Lighting (Backlight section on LP50xx chain) ---
const int BACKLIGHT_FIRST_LED = 65;
const int BACKLIGHT_LAST_LED = 96;
const int BACKLIGHT_LED_COUNT = BACKLIGHT_LAST_LED - BACKLIGHT_FIRST_LED + 1;
enum BackgroundMode { BG_OFF, BG_SOLID, BG_RGB };
BackgroundMode backgroundMode = BG_SOLID;
Color backgroundSolidColor = {0, 50, 0};
int rainbowHue = 0;

const Color BUTTON_ACTIVE_COLOR = {50, 50, 50};
const Color BUTTON_INACTIVE_COLOR = {0, 0, 0};


// --- LP50xx Register Definitions ---
const byte DEVICE_CONFIG0  = 0x00;
const byte OUT0_COLOR_ADDR = 0x14;


// --- Input Device Structs ---
struct EncoderInfo {
  const char* name;
  uint8_t btn_pin, rotA_pin, rotB_pin;
  int startLed;
  const int* ledOrder;
  uint8_t ledOrderLength;
  InterruptEncoder driver;
  long lastDetentPosition;
  bool isPressed;
  bool isMuted; // For toggle functionality
  uint8_t lastButtonState;
  unsigned long lastDebounceTime;
  Color zeroColor;
  Color fullColor;

  EncoderInfo(const char* n, uint8_t b, uint8_t ra, uint8_t rb, int sLed, const int* order, uint8_t orderLen) :
    name(n), btn_pin(b), rotA_pin(ra), rotB_pin(rb), startLed(sLed), ledOrder(order), ledOrderLength(orderLen) {
      lastDetentPosition = 0;
      isPressed = false;
      isMuted = false;
      lastButtonState = HIGH;
      lastDebounceTime = 0;
      zeroColor = {50, 0, 0}; // Default Red
      fullColor = {0, 50, 0}; // Default Green
  }

  void beginEncoder() {
    driver.attach(rotA_pin, rotB_pin);
    driver.count = 0;
  }

  long getRawCount() {
    return driver.read() / 2; // InterruptEncoder::read() reports twice the actual detent count
  }

  void setRawCount(long value) {
    driver.count = value;
  }
};

struct ButtonInfo {
  const char* name;
  uint8_t pin;
  int ledNum;
  uint8_t lastState;
  unsigned long lastDebounceTime;

  ButtonInfo(const char* n, uint8_t p, int ln) : name(n), pin(p), ledNum(ln) {
    lastState = HIGH;
    lastDebounceTime = 0;
  }
};

// --- Input Device Definitions ---
const uint8_t SDA_PIN = 8;
const uint8_t SCL_PIN = 9;
const int MUX_SELECT_PIN = 42;

EncoderInfo encoders[] = {
  EncoderInfo("E1", 4, 5, 6, 1,  ENCODER_LED_ORDER_E1, ENCODER_LED_COUNT),
  EncoderInfo("E2", 7, 10, 11, 11, ENCODER_LED_ORDER_E2, ENCODER_LED_COUNT),
  EncoderInfo("E3", 12, 13, 14, 21, ENCODER_LED_ORDER_E3, ENCODER_LED_COUNT),
  EncoderInfo("E4", 15, 16, 17, 31, ENCODER_LED_ORDER_E4, ENCODER_LED_COUNT),
  EncoderInfo("E5", 18, 1, 2, 41, ENCODER_LED_ORDER_E5, ENCODER_LED_COUNT),
  EncoderInfo("E6", 21, 35, 36, 51, ENCODER_LED_ORDER_E6, ENCODER_LED_COUNT)
};

ButtonInfo buttons[] = {
  ButtonInfo("Ror", 38, 61), ButtonInfo("Rol", 37, 62),
  ButtonInfo("Rur", 40, 63), ButtonInfo("Rul", 41, 64)
};

const int numEncoders = sizeof(encoders) / sizeof(EncoderInfo);
const int numButtons = sizeof(buttons) / sizeof(ButtonInfo);
int selectedOutputIndex = -1;

// --- Function Prototypes ---
void setSingleLedColor(int ledNum, const Color& c);
void updateEncoderLedDisplay(int encoderIndex);
void handleSerialCommands();
void sendEncoderValues();
void updateBackgroundLighting();
Color hexToColor(String hex);
Color Wheel(byte WheelPos);

double encoderCountToVolume(long rawCount);
long volumeToEncoderCount(double volume);
void applyOutputSelection(int index, bool notifySerial);

double encoderCountToVolume(long rawCount) {
  double volume = (-rawCount) * ENCODER_VOLUME_PER_COUNT;
  return volume;
}

long volumeToEncoderCount(double volume) {
  if (ENCODER_VOLUME_PER_COUNT <= 0.0f) {
    return 0;
  }
  double counts = -volume / ENCODER_VOLUME_PER_COUNT;
  return (long)round(counts);
}

void applyOutputSelection(int index, bool notifySerial) {
  if (index < 0 || index >= numButtons) {
    return;
  }

  int previousIndex = selectedOutputIndex;
  selectedOutputIndex = index;

  for (int i = 0; i < numButtons; i++) {
    bool isSelected = (i == selectedOutputIndex);
    setSingleLedColor(buttons[i].ledNum, isSelected ? BUTTON_ACTIVE_COLOR : BUTTON_INACTIVE_COLOR);
  }

  if (notifySerial && previousIndex != selectedOutputIndex) {
    Serial.print("O:");
    Serial.println(selectedOutputIndex + 1);
  }
}


// --- Main Setup ---
void setup() {
  Serial.begin(SERIAL_BAUD_RATE);
  // Quick boot marker to verify serial baud and monitor readability
  delay(50);
  Serial.println("=== deej boot (Serial "+ String(SERIAL_BAUD_RATE) + ") ===");
  Wire.begin(SDA_PIN, SCL_PIN);

  pinMode(MUX_SELECT_PIN, OUTPUT);
  for (int bank = 0; bank < 2; bank++) {
    digitalWrite(MUX_SELECT_PIN, bank == 0 ? LOW : HIGH);
    for (byte address : LED_CHIP_ADDRESSES) {
      Wire.beginTransmission(address); Wire.write(DEVICE_CONFIG0); Wire.write(0x40); Wire.endTransmission();
    }
  }
  digitalWrite(MUX_SELECT_PIN, LOW);

  for(int i = 1; i <= TOTAL_LEDS; i++) { setSingleLedColor(i, {0,0,0}); }

  for (int i = 0; i < numEncoders; i++) {
    encoders[i].beginEncoder();
    encoders[i].setRawCount(0);
    pinMode(encoders[i].btn_pin, INPUT_PULLUP);
    updateEncoderLedDisplay(i);
  }
  for (int i = 0; i < numButtons; i++) { pinMode(buttons[i].pin, INPUT_PULLUP); }

  applyOutputSelection(0, true);
}

// --- Main Loop ---
void loop() {

  // Check Rotary Encoders
  for (int i = 0; i < numEncoders; i++) {
    if (ENCODER_VOLUME_PER_COUNT <= 0.0f) {
      continue; // Prevent division by zero if misconfigured
    }

    long rawCount = encoders[i].getRawCount();
    double requestedVolume = encoderCountToVolume(rawCount);
    double clampedVolume = constrain(requestedVolume, 0.0, (double)MAX_ENCODER_VALUE);

    if (requestedVolume != clampedVolume) {
      encoders[i].setRawCount(volumeToEncoderCount(clampedVolume));
    }

    long currentDetentPosition = (long)round(clampedVolume);

    if (currentDetentPosition != encoders[i].lastDetentPosition) {
      encoders[i].lastDetentPosition = currentDetentPosition;
      updateEncoderLedDisplay(i);
    }
  }

  // Check Encoder Buttons (no deej action, just local LED change)
  for (int i = 0; i < numEncoders; i++) {
    int reading = digitalRead(encoders[i].btn_pin);
    if (reading != encoders[i].lastButtonState && millis() - encoders[i].lastDebounceTime > DEBOUNCE_DELAY) {
      encoders[i].lastDebounceTime = millis();
      encoders[i].lastButtonState = reading;
      if (reading == LOW) { // Button pressed
        encoders[i].isMuted = !encoders[i].isMuted;
        updateEncoderLedDisplay(i);
      }
    }
  }

  // Check Rubber Dome Buttons (no deej action)
  for (int i = 0; i < numButtons; i++) {
    int reading = digitalRead(buttons[i].pin);
    if (reading != buttons[i].lastState && millis() - buttons[i].lastDebounceTime > DEBOUNCE_DELAY) {
      buttons[i].lastDebounceTime = millis();
      buttons[i].lastState = reading;
      if (reading == LOW) {
        applyOutputSelection(i, true);
      }
    }
  }
  
  handleSerialCommands();
  updateBackgroundLighting();
  sendEncoderValues();
  delay(10);
}

// --- Deej Communication ---
void sendEncoderValues() {
  String builtString = "";
  for (int i = 0; i < numEncoders; i++) {
    // Map the 0-MAX_ENCODER_VALUE range to deej's 0-1023 range
    int valueToSend = encoders[i].isMuted ? 0 : encoders[i].lastDetentPosition;
    int deejValue = map(valueToSend, 0, MAX_ENCODER_VALUE, 0, 1023);
    builtString += String(deejValue);
    if (i < numEncoders - 1) {
      builtString += "|";
    }
  }
  Serial.println(builtString);
}

void handleSerialCommands() {
  while (Serial.available() > 0) {
    char c = Serial.read();
    if (c == '\n') {
      // Command format: "ID:Payload"
      int colonPos = serialBuffer.indexOf(':');
      if (colonPos > 0) {
        char commandID = serialBuffer.charAt(0);
        String payload = serialBuffer.substring(colonPos + 1);
        
        if (commandID == 'V') { // Volume update: V:encoderIndex:volume(0.0-1.0)
          int secondColonPos = payload.indexOf(':');
          if (secondColonPos > 0) {
            int encoderIndex = payload.substring(0, secondColonPos).toInt();
            float volume = payload.substring(secondColonPos + 1).toFloat();
            if (encoderIndex >= 0 && encoderIndex < numEncoders) {
              encoders[encoderIndex].lastDetentPosition = (long)round(volume * MAX_ENCODER_VALUE);
              encoders[encoderIndex].setRawCount(volumeToEncoderCount(encoders[encoderIndex].lastDetentPosition));
              updateEncoderLedDisplay(encoderIndex);
            }
          }
        } else if (commandID == 'C') { // Color update: C:encoderIndex:zeroHex:fullHex
          int secondColonPos = payload.indexOf(':');
          int thirdColonPos = payload.lastIndexOf(':');
          if (secondColonPos > 0 && thirdColonPos > secondColonPos) {
            int encoderIndex = payload.substring(0, secondColonPos).toInt();
            String zeroHex = payload.substring(secondColonPos + 1, thirdColonPos);
            String fullHex = payload.substring(thirdColonPos + 1);
            if (encoderIndex >= 0 && encoderIndex < numEncoders) {
              encoders[encoderIndex].zeroColor = hexToColor(zeroHex);
              encoders[encoderIndex].fullColor = hexToColor(fullHex);
              updateEncoderLedDisplay(encoderIndex);
            }
          }
        } else if (commandID == 'B') { // Background lighting: B:rgb or B:hexcolor
          if (payload.equalsIgnoreCase("rgb")) {
            backgroundMode = BG_RGB;
          } else {
            backgroundMode = BG_SOLID;
            Color c = hexToColor(payload);
            backgroundSolidColor = c;
          }
        } else if (commandID == 'O') { // Output device select: O:index(1-4)
          int requestedIndex = payload.toInt() - 1;
          if (requestedIndex >= 0 && requestedIndex < numButtons) {
            applyOutputSelection(requestedIndex, false);
          }
        }
      }
      serialBuffer = "";
    } else {
      serialBuffer += c;
    }
  }
}

// --- LED Control Functions ---
void updateEncoderLedDisplay(int encoderIndex) {
  EncoderInfo& enc = encoders[encoderIndex];

  float volumePercent = (float)enc.lastDetentPosition / MAX_ENCODER_VALUE;
  int ledsToLight = round(volumePercent * ENCODER_LED_COUNT);

  for (int i = 1; i <= ENCODER_LED_COUNT; i++) {
    int globalLedNum = enc.startLed + i - 1;
    setSingleLedColor(globalLedNum, {0,0,0});
  }

  if (enc.isMuted) {
    for (int i = 0; i < ledsToLight; i++) {
      int localLedIndex = enc.ledOrder ? enc.ledOrder[i] : (i + 1);
      int globalLedNum = enc.startLed + localLedIndex - 1;
      setSingleLedColor(globalLedNum, {50, 0, 0});
    }
    return;
  }

  for (int i = 0; i < ledsToLight; i++) {
    int localLedIndex = enc.ledOrder ? enc.ledOrder[i] : (i + 1);
    int globalLedNum = enc.startLed + localLedIndex - 1;
    
    // Calculate color based on position in the lit segment
    float segmentPercent = (float)i / (ENCODER_LED_COUNT - 1);
    if (ledsToLight == 1) segmentPercent = 0; // Avoid division by zero if only one LED is on
    
    Color finalColor = lerp(enc.zeroColor, enc.fullColor, segmentPercent);

    setSingleLedColor(globalLedNum, finalColor);
  }
}

void setSingleLedColor(int ledNum, const Color& c) {
  if (ledNum < 1 || ledNum > TOTAL_LEDS) return;

  int bankIndex = (ledNum - 1) / LEDS_PER_BANK;
  digitalWrite(MUX_SELECT_PIN, bankIndex == 0 ? LOW : HIGH);

  int ledNumInBank = (ledNum - 1) % LEDS_PER_BANK;
  int chipIndexInBank = ledNumInBank / LEDS_PER_CHIP;
  byte chipAddress = LED_CHIP_ADDRESSES[chipIndexInBank];
  int ledNumOnChip = ledNumInBank % LEDS_PER_CHIP;
  int baseOutput = ledNumOnChip * 3;

  Wire.beginTransmission(chipAddress);
  Wire.write(OUT0_COLOR_ADDR + baseOutput);
  Wire.write(c.r); Wire.write(c.g); Wire.write(c.b);
  Wire.endTransmission();
}

void updateBackgroundLighting() {
  switch (backgroundMode) {
    case BG_RGB:
      for (int i = 0; i < BACKLIGHT_LED_COUNT; i++) {
        int ledNum = BACKLIGHT_FIRST_LED + i;
        Color c = Wheel(((i * 256 / BACKLIGHT_LED_COUNT) + rainbowHue) & 255);
        setSingleLedColor(ledNum, c);
      }
      rainbowHue++;
      if (rainbowHue >= 256 * 5) rainbowHue = 0;
      break;
    case BG_SOLID:
      for (int i = 0; i < BACKLIGHT_LED_COUNT; i++) {
        int ledNum = BACKLIGHT_FIRST_LED + i;
        setSingleLedColor(ledNum, backgroundSolidColor);
      }
      break;
    case BG_OFF:
    default:
      for (int i = 0; i < BACKLIGHT_LED_COUNT; i++) {
        int ledNum = BACKLIGHT_FIRST_LED + i;
        setSingleLedColor(ledNum, {0, 0, 0});
      }
      break;
  }
}

// --- Utility Functions ---
Color hexToColor(String hex) {
  hex.remove(0, hex.startsWith("#") ? 1 : 0);
  long number = strtol(hex.c_str(), NULL, 16);
  byte r = (number >> 16) & 0xFF;
  byte g = (number >> 8) & 0xFF;
  byte b = number & 0xFF;
  return {r, g, b};
}

Color Wheel(byte WheelPos) {
  WheelPos = 255 - WheelPos;
  if(WheelPos < 85) {
    return { (byte)(255 - WheelPos * 3), 0, (byte)(WheelPos * 3) };
  }
  if(WheelPos < 170) {
    WheelPos -= 85;
    return { 0, (byte)(WheelPos * 3), (byte)(255 - WheelPos * 3) };
  }
  WheelPos -= 170;
  return { (byte)(WheelPos * 3), (byte)(255 - WheelPos * 3), 0 };
}
