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


## Connecting

The ardubdm PCB has a shrouded 2x5 box header with the standard CPU32 BDM
pinout (1 DS, 2 BERR, 3 GND, 4 BKPT, 5 GND, 6 FREEZE, 7 RESET, 8 DSI, 9 VDD,
10 DSO). The 10-way IDC cable is wired 1:1; the red stripe is pin 1. At the
adapter the plug only fits one way.

The T5 and T7 have a bare 2x4 header carrying BDM pins 3-10. The 2x5 plug is
one column wider, so one column of holes stays empty: **always holes 1 and 2,
the column at the red stripe**. Holes 3-10 go on the eight pins. Numbers below
are the plug's holes, in parentheses = empty, hanging next to the header.

Hold the ECU with the BDM header side towards you: the big edge connector
and its heatsink at the far edge, the CPU (the large square chip) near you,
and the 2x4 BDM header just below the CPU at the near edge. T5 and T7 look
the same in this respect.

```
   +----------------------------------------------------+
   |##### edge connector / heatsink (far edge) ##########|
   |                                                     |
   |                +-----------+                        |
   |                |   68332   |                        |
   |                |    CPU    |                        |
   |                +-----------+                        |
   |                 o o o o                             |
   |                 o o o o  <- BDM                     |
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

Ribbon leaves **upwards**. Empty holes 1 and 2 on the **left**:

```
      ribbon up
        |||||
        |||||
   (2)  4  6  8  10     <- even row
   (1)  3  5  7   9     <- odd row, red stripe at hole 1
    ^   ^^^^^^^^^^^
  empty on the 2x4 header
```

The column with holes 9/10 (VDD, DSO) is at the board edge. Compared with
the old cable (which had GND on pin 1 and sat with its holes 9/10 past the
board edge), the plug keeps the same orientation and sits one column further
in.
