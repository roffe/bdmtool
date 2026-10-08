package bdm

import (
	"bytes"
	"errors"
	"testing"
)

func TestToyFraming(t *testing.T) {
	// The bdmtoy host's TAP_SetInterface for old BDM at 1 MHz, queued alone:
	// [8 words][1 command] [0001][6] [type 0][cfg 8000][freq 000F4240 low word first]
	got := toyFrameOut([]uint16{toySetInterface, toyBDMOld, toyBigEndian, 0x4240, 0x000f})
	want := []byte{8, 0, 1, 0, 1, 0, 6, 0, 0, 0, 0, 0x80, 0x40, 0x42, 0x0f, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame % X, want % X", got, want)
	}

	// Two replies: a write (no data), then a register read of 0x12345678.
	r, err := toyReplies([]uint16{9, toyWriteMem, 0, 3, toyReadReg, 0, 5, 0x5678, 0x1234}, 2)
	if err != nil || len(r) != 2 || len(r[0]) != 0 || r[1][0] != 0x5678 || r[1][1] != 0x1234 {
		t.Fatalf("replies %v, %v", r, err)
	}
	// The adapter stops at a failed command: bus error on the first.
	if _, err := toyReplies([]uint16{4, toyReadMem, 0xF020, 3}, 2); err == nil || err.Error() != "bdmtoy command 0040: bus error" {
		t.Fatalf("bus error reply: %v", err)
	}
	if _, err := toyReplies([]uint16{4, toyReadMem, 0, 9}, 1); err == nil {
		t.Fatal("overlong reply length accepted")
	}

	// Target long words come back low word first.
	if b := toyBytes([]uint16{0xEFFC, 0xFFFF, 0x8A7C, 0x0005}); !bytes.Equal(b, []byte{0xFF, 0xFF, 0xEF, 0xFC, 0, 5, 0x8A, 0x7C}) {
		t.Fatalf("toyBytes % X", b)
	}
	// Firmware before 1.0 answers the version command "not supported".
	if !errors.Is(toyErr(toyVersion, 0xF000), errToyNotSup) {
		t.Fatal("not-supported status not recognised")
	}

	// The fast clock only where the prep sets 16 MHz or more.
	for name, want := range map[string]uint32{
		"Trionic 7": toyFastClock, "Trionic 5.2": toyFastClock, "MC68331": toyFastClock,
		"Trionic 8": toySlowClock, "Volvo CEM": toyFastClock,
	} {
		if got := toyBulkClock(ecuByName(name)); got != want {
			t.Errorf("%s: clock %d, want %d", name, got, want)
		}
	}
}
