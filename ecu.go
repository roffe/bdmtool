package main

import "bytes"

// ECU descriptors and the per-ECU register setup ("prepare") sequences,
// transcribed from the CombiAdapter .NET driver (caAdapterBase::ECUDescriptors
// and caAdapterBase::prepare_ecu). The writes configure the MCU's chip
// selects, bus timing and watchdog so external flash is addressable while the
// core sits in background mode.

type memWrite struct {
	addr uint32
	val  uint32
	size int // bytes: 1, 2 or 4; 0 means "pause val milliseconds", see delay()
}

type ECU struct {
	Name          string
	FlashType     string // exactly 6 chars; sent to the adapter's flash driver
	FlashAddr     uint32
	FlashSize     uint32
	SRAMAddr      uint32
	SRAMSize      uint32
	EraseFeedback bool // 28Fxxx chips report per-word erase progress
	Prepare       []memWrite
	// Where the CPU32 flash driver runs and what maps that RAM. The MCU's
	// internal RAM is used, not the ECU's SRAM: it needs no chip selects, so
	// it works after a reset into BDM when the ECU's own setup never ran.
	DrvAddr uint32
	DrvPrep []memWrite
}

// 68332 standby RAM (2 KB) mapped at 0x100000 via RAMBAR, as Just4Trionic does.
var ram332 = []memWrite{w16(0xfffb04, 0x1000)}

// 68377 DPTRAM (6 KB) mapped at 0x100000, per Just4Trionic's T8 prep.
var ram377 = []memWrite{w16(0xfff684, 0x1000)}

// words expands a run of consecutive 16-bit writes starting at addr.
func words(addr uint32, vals ...uint32) []memWrite {
	out := make([]memWrite, len(vals))
	for i, v := range vals {
		out[i] = memWrite{addr + uint32(2*i), v, 2}
	}
	return out
}

func w16(addr, val uint32) memWrite { return memWrite{addr, val, 2} }

// delay pauses in the middle of a prepare sequence: the MCP's PLL needs time
// to relock after SYNCR is written before the bus can be touched again.
func delay(ms uint32) memWrite     { return memWrite{0, ms, 0} }
func w8(addr, val uint32) memWrite { return memWrite{addr, val, 1} }

// Trionic 5.2, 5.5 (28F010 chips) and Volvo CEM share this setup.
var prepT5 = []memWrite{
	w16(0xfffa04, 0x7f00), w16(0xfffa21, 0x0000), w16(0xfffa44, 0x3fff),
	w16(0xfffa48, 0x0405), w16(0xfffa4a, 0x6b70), w16(0xfffa50, 0x0405),
	w16(0xfffa52, 0x3370), w16(0xfffa54, 0x0405), w16(0xfffa56, 0x5370),
	w16(0xfffc14, 0x0040), w16(0xfffc17, 0x0040),
}

var prepT55New = []memWrite{
	w16(0xfffa20, 0x000c), w16(0xfffa00, 0x40cf), w16(0xfffa04, 0x7f00),
	w16(0xfffa21, 0x0000), w16(0xfffa44, 0x3fff), w16(0xfffa4a, 0x6b70),
	w16(0xfffa50, 0x0005), w16(0xfffa52, 0x3370), w16(0xfffa54, 0x0005),
	w16(0xfffa56, 0x5370),
}

var prepT7 = concat(
	[]memWrite{
		w16(0xfffa00, 0x61c2), w16(0xfffa04, 0x7f08), w16(0xfffa11, 0x0000),
		w16(0xfffa13, 0x0000), w16(0xfffa15, 0x00fe), w16(0xfffa17, 0x0011),
		w16(0xfffa19, 0x007b), w16(0xfffa1b, 0x007b), w16(0xfffa1d, 0x0085),
		w16(0xfffa1f, 0x0008), w16(0xfffa22, 0x0250), w16(0xfffa24, 0x0129),
		w16(0xfffa27, 0x0000), w16(0xfffa41, 0x000f), w16(0xfffa44, 0x2fff),
		w16(0xfffa46, 0x0001), w16(0xfffa17, 0x0011), w16(0xfffa48, 0x0006),
	},
	words(0xfffa4c,
		0xf003, 0x6830, 0x0006, 0x1030, 0x0000, 0x0000, 0xf003, 0x5030,
		0xf003, 0x3030, 0xff00, 0x7bf0, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0x0000, 0xfff8, 0x2bc7, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0x0000),
	[]memWrite{w16(0xfffa4a, 0x6bb0)},
	words(0xfffa4e,
		0x6830, 0x0006, 0x1030, 0x0000, 0x0000, 0xf003, 0x5030, 0xf003,
		0x3030, 0xff00, 0x7bf0, 0x0000, 0x0000, 0x0000, 0x0000, 0x0000,
		0x0000, 0xfff8, 0x2bc7),
	[]memWrite{
		w16(0xfffa04, 0x7f00), w8(0xfffa21, 0x00), w16(0xfffa4a, 0x6b70),
		w16(0xfffa50, 0x0007), w16(0xfffa52, 0x3370),
	},
)

// Trionic 8 MCP (MC68F375 coprocessor): flash, SRAM and DPTRAM are all on-chip
// and unmapped after a reset into BDM, so this maps them and cranks the PLL to
// the 24 MHz the CMFI driver's built-in timing assumes. From bdmtoy's
// trionic_8mcp::init().
var prepT8MCP = []memWrite{
	w8(0xfffa21, 0x70),    // SYPCR - watchdog off, slowest prescale (the MCP ignores the disable bit)
	w16(0xfffa04, 0xd080), // SYNCR - 24 MHz
	delay(5),              // PLL relock
	w16(0xfff800, 0xc800), // CMFIMCR - STOP + PROTECT, LOCK off
	w16(0xfff808, 0x0000), // CMFIBAH - flash base 00_0000
	w16(0xfff80a, 0x0000), // CMFIBAL
	w16(0xfff800, 0x4800), // CMFIMCR - flash enabled
	w16(0xfff884, 0x1000), // DPTBAR  - DPTRAM at 10_0000 (where the driver runs)
	w16(0xfff880, 0x0000), // DPTMCR  - DPTRAM enabled
	w16(0xfffb04, 0x0020), // RAMBAH  - SRAM at 20_0000
	w16(0xfffb06, 0x0000), // RAMBAL
	w16(0xfffb00, 0x0800), // RAMMCR  - SRAM enabled
}

// GM CANdi module: MC68331, Am29F200B flash (256 KB) at 0, two K6X1008 SRAMs
// (256 KB) at 0x100000. Register values read out of the module's own firmware,
// via bdmtoy's initCandi(). CSORBT allows writes, so the flash unlock sequence
// goes through CSBOOT and no second window is needed.
var prepCandi = []memWrite{
	w16(0xfffa00, 0x40cf), // SIMCR   - modules at 0xFFFxxx (MM = 1)
	w16(0xfffa04, 0xd608), // SYNCR   - clock synthesiser, this board
	delay(5),              // PLL relock
	w8(0xfffa21, 0x00),    // SYPCR   - watchdog off
	w16(0xfffa44, 0x00ff), // CSPAR0
	w16(0xfffa46, 0x00a9), // CSPAR1
	w16(0xfffa48, 0x0005), // CSBARBT - flash at 00_0000, 256K
	w16(0xfffa4a, 0x7870), // CSORBT  - both byte lanes, read and write, 1 wait state
	w16(0xfffa4c, 0x1005), // CSBAR0  - SRAM at 10_0000, 256K
	w16(0xfffa4e, 0x5830), // CSOR0   - byte lane A
	w16(0xfffa50, 0x1005), // CSBAR1  - SRAM at 10_0000
	w16(0xfffa52, 0x3830), // CSOR1   - byte lane B
	w8(0xfffa1f, 0x00),    // PFPAR   - port F as the firmware leaves it
	w8(0xfffa1d, 0x00),    // DDRF
}

var ECUs = []ECU{
	{"Trionic 5.2", "28f010", 0x60000, 0x20000, 0x0, 0x8000, true, prepT5, 0x100000, ram332},
	{"Trionic 5.5 (28F010 chips)", "28f010", 0x40000, 0x40000, 0x0, 0x8000, true, prepT5, 0x100000, ram332},
	{"Trionic 5.5 (AM29F010 chips)", "29f010", 0x40000, 0x40000, 0x0, 0x8000, false, prepT55New, 0x100000, ram332},
	{"Trionic 7", "29f400", 0x0, 0x80000, 0xf00000, 0xffff, false, prepT7, 0x100000, ram332},
	{"Trionic 8", "29f400", 0x0, 0x100000, 0xf00000, 0xffff, false, []memWrite{w16(0xfffa50, 0x0000)}, 0x100000, ram377},
	{"Trionic 8 MCP", "cmfi", 0x0, 0x40100, 0x200000, 0x2000, false, prepT8MCP, 0x100000, nil},
	// The flash driver runs from the module's external SRAM, which prepCandi
	// maps: the 68331's TPU RAM would land on top of that SRAM at 0x100000.
	{"MC68331", "29f400", 0x0, 0x40000, 0x100000, 0x40000, false, prepCandi, 0x100000, nil},
	{"Volvo CEM", "28f400", 0x0, 0x80000, 0xf00000, 0xffff, false, prepT5, 0, nil},
}

// mirror repeats a smaller image to fill size, as bdmtoy does: a T5.2 fitted
// with 28F010 chips boots from 0 but runs at 0x60000, two different halves
// of the chips, so its 128 KB image has to sit in both. Anything that does
// not divide size evenly is returned as is for the size check to reject.
func mirror(bin []byte, size uint32) []byte {
	if n := uint32(len(bin)); n > 0 && n < size && size%n == 0 {
		return bytes.Repeat(bin, int(size/n))
	}
	return bin
}

func ecuByName(name string) *ECU {
	for i := range ECUs {
		if ECUs[i].Name == name {
			return &ECUs[i]
		}
	}
	return nil
}

func concat[T any](s ...[]T) []T {
	var out []T
	for _, v := range s {
		out = append(out, v...)
	}
	return out
}
