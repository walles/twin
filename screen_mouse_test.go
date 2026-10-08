package twin

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestShouldEnableMouseTracking(t *testing.T) {
	for _, testCase := range []struct {
		name                  string
		mouseMode             MouseMode
		alternateScroll       alternateScrollSupport
		hasArrowKeysEmulation bool

		expected              bool
		expectTerminalChecked bool // Whether hasArrowKeysEmulation() should be called
	}{
		// Explicit mouse modes win over everything else
		{"Scroll", MouseModeScroll, alternateScrollUnknown, true, true, false},
		{"Scroll, Alternate Scroll supported", MouseModeScroll, alternateScrollSupported, true, true, false},
		{"Select", MouseModeSelect, alternateScrollUnknown, false, false, false},
		{"Select, Alternate Scroll unsupported", MouseModeSelect, alternateScrollUnsupported, false, false, false},

		// Alternate Scroll Mode makes the mouse wheel work without mouse
		// tracking, no need to check terminal specifics
		{"Auto, supported", MouseModeAuto, alternateScrollSupported, false, false, false},

		// Some terminals emulate arrow keys without supporting Alternate Scroll
		// Mode, so fall back to checking terminal specifics
		{"Auto, unsupported, emulation", MouseModeAuto, alternateScrollUnsupported, true, false, true},
		{"Auto, unsupported, no emulation", MouseModeAuto, alternateScrollUnsupported, false, true, true},
		{"Auto, unknown, emulation", MouseModeAuto, alternateScrollUnknown, true, false, true},
		{"Auto, unknown, no emulation", MouseModeAuto, alternateScrollUnknown, false, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			terminalChecked := false
			hasArrowKeysEmulation := func() bool {
				terminalChecked = true
				return testCase.hasArrowKeysEmulation
			}

			actual := shouldEnableMouseTracking(testCase.mouseMode, testCase.alternateScroll, hasArrowKeysEmulation)

			assert.Equal(t, actual, testCase.expected)
			assert.Equal(t, terminalChecked, testCase.expectTerminalChecked)
		})
	}
}
