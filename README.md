# BDMTool

![BDMTool main window](screenshot.png)

Reads and writes SAAB Trionic (and Volvo CEM) ECU flash and SRAM over BDM
(Background Debug Mode). A extended Go/Fyne version of Janis Silins' BDM Tool.

## Supported adapters

- CombiAdapter (incl. MkII and STM32 clones)
- USB BDM
- USB BDM MkII
- ardubdm (ATmega328PB over USB serial; one entry per serial port found at startup)

## Supported ECUs

- Trionic 5.2
- Trionic 5.5 (28F010 chips)
- Trionic 5.5 (AM29F010 chips)
- Trionic 7
- Trionic 8
- Volvo CEM

## Build and run

Requires Go and libusb-1.0 development headers (CombiAdapter and USB BDM use libusb).

```sh
go build -o bdmtool .
./bdmtool
```

To regenerate the screenshot above (renders offscreen, no display needed):

```sh
./bdmtool -screenshot screenshot.png
```

## Credits

- Go/Fyne port by Joakim "Roffe" Karlsson.
- Original BDM Tool v2.13 by Janis Silins, 2009-2016.
- Portions of code from BDM v0.90, Scott Howard, 1992.
- Flash routines transcribed from Just4Trionic (Sophie Dexter).

For non-commercial use only.

If you like the software please [donate](https://paypal.me/roffe84) 💕


## Identify ECU

With an ardubdm connected, the "Identify ECU" button first halts the running
ECU and decodes its 68332 SIM registers into the log: CPU clock from SYNCR
(16.78 MHz on a prepped T7), the reason for the last reset (RSR: power-on,
external, watchdog, halt after a double bus fault, loss of clock), watchdog
and bus monitor settings (SYPCR), and the chip-select memory map (base, size,
read/write, byte lanes, wait states). If the ECU cannot be halted it resets
into BDM and reports the reset defaults, saying so.

It then resets into BDM and reads the flash chip IDs the way Just4Trionic
does (a 68377 CPU is reported as Trionic 8 before any flash probe): 29F400 =
Trionic 7, a pair of 28F010 or 29F010 = Trionic 5.5, a pair of 28F512 =
Trionic 5.2. The matching ECU type is selected and the chips are logged, and
the ECU is then reset and left running its own code (with BDM enabled), so a
second Identify shows its real configuration rather than the probe's.

`ARDUBDM_PORT=/dev/ttyACM1 go test -run Hardware -v` runs the same against a
connected adapter from the command line.

## Connecting

The ardubdm PCB has a shrouded 2x5 box header with the standard CPU32 BDM pinout 

    | BDM pin | Signal      | Arduino |
    |---------|-------------|---------|
    | 1       | DS          | D8      |
    | 2       | BERR        | D6      |
    | 3       | GND         | GND     |
    | 4       | BKPT/DSCLK  | D5      |
    | 5       | GND         | GND     |
    | 6       | FREEZE      | D4      |
    | 7       | RESET       | D7      |
    | 8       | IFETCH/DSI  | D3      |
    | 9       | VDD         | A0      |
    | 10      | IPIPE/DSO   | D2      |

The 10-way IDC cable is wired 1:1; the red stripe is pin 1. At the
adapter the plug only fits one way.

The T7 has the same standard 2x5 header, all ten pins present (BERR on pin 2
is wired to the MCU), so the plug goes on pin for pin. 

The T5 has pins 1 and 2 unused, so there one column of plug holes stays empty: **holes 1 and 2, the
column at the red stripe**, and holes 3-10 go on the pins. Numbers below are
the plug's holes, in parentheses = empty, hanging next to the header.

Hold the ECU with the BDM header side towards you: the big edge connector
and its heatsink at the far edge, the CPU (the large square chip) near you,
and the BDM header just below the CPU at the near edge. T5 and T7 look the
same in this respect.

```
   +----------------------------------------------------+
   |##### edge connector / heatsink (far edge) ##########|
   |                                                     |
   |                +-----------+                        |
   |                |   68332   |                        |
   |                |    CPU    |                        |
   |                +-----------+                        |
   |                 o o o o o                           |
   |                 o o o o o  <- BDM                   |
   +----------------------------------------------------+
                   near edge, towards you
```

### Trionic 5

Ribbon leaves **downwards**. Empty holes 1 and 2 on the **right**. Same plug,
turned half a turn compared with the T7:

```
     9  7  5  3  (1)    <- odd row, red stripe at hole 1
    10  8  6  4  (2)    <- even row
    ^^^^^^^^^^^   ^
  on the 2x4 header  empty
        |||||
        |||||        
     ribbon down
```

### Trionic 7

Ribbon leaves **upwards**. All ten holes on the pins, red stripe on the
**left**:

```
      ribbon up
        |||||
        |||||
    2  4  6  8  10      <- even row
    1  3  5  7   9      <- odd row, red stripe at hole 1
    ^^^^^^^^^^^^^^
```
