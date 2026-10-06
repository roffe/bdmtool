package bdm

import (
	"strings"
	"testing"
)

// simRegs is a prober over a register map; Info only halts and reads.
type simRegs map[uint32]uint32

func (m simRegs) Restart() error                          { return nil }
func (m simRegs) Stop() error                             { return nil }
func (m simRegs) Run(uint32) error                        { return nil }
func (m simRegs) setFunctionCode(uint32) error            { return nil }
func (m simRegs) writeMem(a, v uint32, _ int) error       { m[a] = v; return nil }
func (m simRegs) readMem(a uint32, _ int) (uint32, error) { return m[a], nil }

// Expected values per the MC68332 user's manual: watchdog ratio / 32.768 kHz,
// bus monitor in system clocks, DSACK 14 = fast termination, 15 = external.
func TestInfoDecode(t *testing.T) {
	m := simRegs{
		0xfffa04: 0x7f08,                   // SYNCR, 16.78 MHz
		0xfffa21: 0xec,                     // SYPCR as the T7 firmware sets it
		0xfffa44: 0x3fff,                   // CSBOOT, CS0 are chip selects
		0xfffa48: 0x0006, 0xfffa4a: 0x6bb0, // CSBOOT, DSACK 14
		0xfffa4c: 0x1003, 0xfffa4e: 0x7bf0, // CS0, DSACK 15
	}
	s, err := info(m)
	if err != nil {
		t.Fatal(err)
	}
	line := func(prefix string) string {
		for l := range strings.Lines(s) {
			if strings.HasPrefix(strings.TrimSpace(l), prefix) {
				return strings.TrimSpace(l)
			}
		}
		t.Fatalf("no %q line in\n%s", prefix, s)
		return ""
	}
	for prefix, want := range map[string]string{
		"SYPCR":  "watchdog on, timeout 2m8s; bus monitor on, 64 clocks (3.8 us); halt monitor on",
		"CSBOOT": "fast termination",
		"CS0":    "external DSACK",
	} {
		if l := line(prefix); !strings.HasSuffix(l, want) {
			t.Errorf("%s line = %q, want suffix %q", prefix, l, want)
		}
	}

	m[0xfffa21] = 0xac // T5 firmware
	if s, _ = info(m); !strings.Contains(s, "timeout 250ms") {
		t.Errorf("T5 SYPCR 0xAC:\n%s", s)
	}
}
