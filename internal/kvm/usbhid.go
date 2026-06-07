package kvm

// USB HID keyboard usage codes and the X11-keysym → usage mapping used by the
// RFB input path.
//
// noVNC delivers X11 keysyms (not Java AWT virtual key codes), so this table is
// keyed by keysym rather than ported verbatim from the Java
// USBKeyProcessorEnglish standardMap (which is AWT-VK keyed). The *USB usage
// values* and the *modifier bitmask layout* are the same as the Java port; only
// the lookup key differs. The modifier bits below match KeyProcessor.java:
//
//	MOD_LEFT_CTRL=1 MOD_LEFT_SHIFT=2 MOD_LEFT_ALT=4 MOD_LEFT_WIN=8
//	MOD_RIGHT_CTRL=16 MOD_RIGHT_SHIFT=32 MOD_RIGHT_ALT=64 MOD_RIGHT_WIN=128
const (
	modLeftCtrl   = 0x01
	modLeftShift  = 0x02
	modLeftAlt    = 0x04
	modLeftGUI    = 0x08
	modRightCtrl  = 0x10
	modRightShift = 0x20
	modRightAlt   = 0x40
	modRightGUI   = 0x80
)

// modifierKeysym maps a keysym to the USB HID modifier bit it toggles. Keys in
// this map do NOT occupy a usage slot in bytes 2..7; they set/clear byte0.
var modifierKeysym = map[uint32]byte{
	0xffe1: modLeftShift,  // XK_Shift_L
	0xffe2: modRightShift, // XK_Shift_R
	0xffe3: modLeftCtrl,   // XK_Control_L
	0xffe4: modRightCtrl,  // XK_Control_R
	0xffe9: modLeftAlt,    // XK_Alt_L
	0xffea: modRightAlt,   // XK_Alt_R  (some hosts send AltGr as ISO_Level3_Shift)
	0xfe03: modRightAlt,   // XK_ISO_Level3_Shift (AltGr)
	0xffeb: modLeftGUI,    // XK_Super_L (Windows/GUI)
	0xffec: modRightGUI,   // XK_Super_R
	0xffe7: modLeftGUI,    // XK_Meta_L
	0xffe8: modRightGUI,   // XK_Meta_R
}

// usbUsage maps a non-modifier keysym to its USB HID keyboard usage code.
// The values match the USB HID Usage Tables (and the Java standardMap values).
// Letters are keyed by their lowercase keysym; noVNC sends the Shift modifier
// separately for uppercase, so both cases land on the same usage code.
var usbUsage = map[uint32]byte{
	// Letters a..z → usage 0x04..0x1d. Both lowercase (0x61..) and uppercase
	// (0x41..) keysyms are inserted programmatically in init().

	// Number row 1..0 → 0x1e..0x27
	'1': 0x1e, '2': 0x1f, '3': 0x20, '4': 0x21, '5': 0x22,
	'6': 0x23, '7': 0x24, '8': 0x25, '9': 0x26, '0': 0x27,

	// Shifted number row — same physical key, Shift sent separately by noVNC.
	'!': 0x1e, '@': 0x1f, '#': 0x20, '$': 0x21, '%': 0x22,
	'^': 0x23, '&': 0x24, '*': 0x25, '(': 0x26, ')': 0x27,

	// Editing / whitespace
	0xff0d: 0x28, // XK_Return / Enter
	0xff1b: 0x29, // XK_Escape
	0xff08: 0x2a, // XK_BackSpace
	0xff09: 0x2b, // XK_Tab
	' ':    0x2c, // space

	// Punctuation (unshifted and shifted variants → same usage code)
	'-': 0x2d, '_': 0x2d, // minus / underscore
	'=': 0x2e, '+': 0x2e, // equal / plus
	'[': 0x2f, '{': 0x2f, // bracket left / brace left
	']': 0x30, '}': 0x30, // bracket right / brace right
	'\\': 0x31, '|': 0x31, // backslash / pipe
	';': 0x33, ':': 0x33, // semicolon / colon
	'\'': 0x34, '"': 0x34, // apostrophe / quote
	'`': 0x35, '~': 0x35, // grave / tilde
	',': 0x36, '<': 0x36, // comma / less
	'.': 0x37, '>': 0x37, // period / greater
	'/': 0x38, '?': 0x38, // slash / question

	0xffe5: 0x39, // XK_Caps_Lock

	// Function keys F1..F12 → 0x3a..0x45
	0xffbe: 0x3a, 0xffbf: 0x3b, 0xffc0: 0x3c, 0xffc1: 0x3d,
	0xffc2: 0x3e, 0xffc3: 0x3f, 0xffc4: 0x40, 0xffc5: 0x41,
	0xffc6: 0x42, 0xffc7: 0x43, 0xffc8: 0x44, 0xffc9: 0x45,

	// System / navigation
	0xff61: 0x46, // XK_Print / PrintScreen
	0xff14: 0x47, // XK_Scroll_Lock
	0xff13: 0x48, // XK_Pause
	0xff63: 0x49, // XK_Insert
	0xff50: 0x4a, // XK_Home
	0xff55: 0x4b, // XK_Prior / Page Up
	0xffff: 0x4c, // XK_Delete
	0xff57: 0x4d, // XK_End
	0xff56: 0x4e, // XK_Next / Page Down
	0xff53: 0x4f, // XK_Right
	0xff51: 0x50, // XK_Left
	0xff54: 0x51, // XK_Down
	0xff52: 0x52, // XK_Up

	0xff7f: 0x53, // XK_Num_Lock

	// Keypad — operators and Enter
	0xffaf: 0x54, // XK_KP_Divide
	0xffaa: 0x55, // XK_KP_Multiply
	0xffad: 0x56, // XK_KP_Subtract
	0xffab: 0x57, // XK_KP_Add
	0xff8d: 0x58, // XK_KP_Enter

	// Keypad digits (numeric, NumLock on) XK_KP_0..XK_KP_9 → 0x62.. then 1..9
	0xffb1: 0x59, 0xffb2: 0x5a, 0xffb3: 0x5b, 0xffb4: 0x5c, 0xffb5: 0x5d,
	0xffb6: 0x5e, 0xffb7: 0x5f, 0xffb8: 0x60, 0xffb9: 0x61, 0xffb0: 0x62,
	0xffae: 0x63, // XK_KP_Decimal
	0xffbd: 0x67, // XK_KP_Equal

	// Application / menu key
	0xff67: 0x65, // XK_Menu
}

func init() {
	// Letters: a..z keysyms 0x61..0x7a and A..Z 0x41..0x5a both → usage 0x04..
	for i := 0; i < 26; i++ {
		usage := byte(0x04 + i)
		usbUsage[uint32('a'+i)] = usage
		usbUsage[uint32('A'+i)] = usage
	}
}

// usageFor returns the USB HID usage code for a non-modifier keysym, or 0 if the
// key is unmapped.
func usageFor(keysym uint32) byte { return usbUsage[keysym] }

// modBitFor returns the USB modifier bit for a modifier keysym, or 0 if the
// keysym is not a modifier.
func modBitFor(keysym uint32) byte { return modifierKeysym[keysym] }

// scancodePassthroughBase tags a keysym that carries a raw USB HID usage code in
// its low byte rather than a character. It backs the client-side "scancode
// pass-through" keyboard mode used for international layouts: the frontend
// (internal/webui/assets/js/keyboard.js) maps each physical KeyboardEvent.code
// to its layout-independent USB usage and sends (scancodePassthroughBase |
// usage), so the *guest's* own keymap — not our US-only table — turns the
// physical key into a character. The base sits far above every real X11 keysym
// (Unicode keysyms top out near 0x0110FFFF, vendor keysyms near 0x1008FFFF), so
// it can never collide with a genuine keysym noVNC would send.
const scancodePassthroughBase = 0xF000_0000

// usbUsageFromKeysym decodes a scancode-pass-through keysym into its raw USB HID
// usage code. ok is false for ordinary keysyms, which take the US-layout path.
func usbUsageFromKeysym(keysym uint32) (usage byte, ok bool) {
	if keysym&0xFFFF_FF00 == scancodePassthroughBase {
		return byte(keysym & 0xFF), true
	}
	return 0, false
}

// modBitForUsage maps a USB HID modifier usage (0xE0..0xE7, per the USB HID
// Usage Tables) to its modifier bit, or 0 for a non-modifier usage. Used to
// route pass-through usages into byte0 instead of a key slot.
func modBitForUsage(usage byte) byte {
	switch usage {
	case 0xE0:
		return modLeftCtrl
	case 0xE1:
		return modLeftShift
	case 0xE2:
		return modLeftAlt
	case 0xE3:
		return modLeftGUI
	case 0xE4:
		return modRightCtrl
	case 0xE5:
		return modRightShift
	case 0xE6:
		return modRightAlt
	case 0xE7:
		return modRightGUI
	}
	return 0
}
