//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	physicsMapName = `Local\acpmf_physics`
	physicsMapSize = 800
	pollInterval   = 100 * time.Millisecond
)

var procOpenFileMappingW = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileMappingW")

type physicsSample struct {
	PacketID int32
	Gas      float32
	Brake    float32
	Fuel     float32
	Gear     int32
	RPM      int32
	Steer    float32
	SpeedKMH float32
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "acr-telemetry: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctxDone := make(chan os.Signal, 1)
	signal.Notify(ctxDone, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ctxDone)

	fmt.Println("ACR telemetry shared-memory probe")
	fmt.Println("Waiting for Assetto Corsa Rally physics map...")

	reader, err := waitForSharedMemory(ctxDone)
	if err != nil {
		return err
	}
	defer reader.Close()

	fmt.Printf("Connected to %s (%d bytes). Press Ctrl+C to stop.\n\n", physicsMapName, physicsMapSize)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	lastPacketID := int32(-1)
	for {
		select {
		case <-ctxDone:
			fmt.Println("Stopping.")
			return nil
		case <-ticker.C:
			sample, err := reader.ReadPhysicsSample()
			if err != nil {
				return err
			}
			if sample.PacketID == lastPacketID {
				continue
			}
			lastPacketID = sample.PacketID
			fmt.Printf("packet=%d speed=%.1f km/h rpm=%d gear=%s throttle=%.0f%% brake=%.0f%% fuel=%.1f L steer=%.2f\n",
				sample.PacketID,
				sample.SpeedKMH,
				sample.RPM,
				formatGear(sample.Gear),
				sample.Gas*100,
				sample.Brake*100,
				sample.Fuel,
				sample.Steer,
			)
		}
	}
}

func waitForSharedMemory(done <-chan os.Signal) (*sharedMemoryReader, error) {
	for {
		reader, err := OpenSharedMemory(physicsMapName, physicsMapSize)
		if err == nil {
			return reader, nil
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, err
		}

		select {
		case <-done:
			return nil, errors.New("interrupted while waiting for shared memory")
		case <-time.After(time.Second):
			fmt.Print(".")
		}
	}
}

func formatGear(gear int32) string {
	switch gear {
	case 0:
		return "R"
	case 1:
		return "N"
	default:
		return fmt.Sprintf("%d", gear-1)
	}
}

type sharedMemoryReader struct {
	handle windows.Handle
	addr   uintptr
	size   int
}

func OpenSharedMemory(name string, size int) (*sharedMemoryReader, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	handle, err := openFileMapping(windows.FILE_MAP_READ, false, namePtr)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}

	addr, err := windows.MapViewOfFile(handle, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("map %s: %w", name, err)
	}

	return &sharedMemoryReader{handle: handle, addr: addr, size: size}, nil
}

func openFileMapping(access uint32, inherit bool, name *uint16) (windows.Handle, error) {
	inheritHandle := uintptr(0)
	if inherit {
		inheritHandle = 1
	}

	r0, _, err := procOpenFileMappingW.Call(uintptr(access), inheritHandle, uintptr(unsafe.Pointer(name)))
	if r0 == 0 {
		if err != syscall.Errno(0) {
			return 0, err
		}
		return 0, windows.ERROR_INVALID_HANDLE
	}
	return windows.Handle(r0), nil
}

func (r *sharedMemoryReader) ReadPhysicsSample() (physicsSample, error) {
	if r == nil || r.addr == 0 {
		return physicsSample{}, errors.New("shared memory is not open")
	}
	if r.size < 32 {
		return physicsSample{}, fmt.Errorf("physics map too small: %d bytes", r.size)
	}

	data := unsafe.Slice((*byte)(unsafe.Pointer(r.addr)), r.size)
	return physicsSample{
		PacketID: int32(binary.LittleEndian.Uint32(data[0:4])),
		Gas:      math.Float32frombits(binary.LittleEndian.Uint32(data[4:8])),
		Brake:    math.Float32frombits(binary.LittleEndian.Uint32(data[8:12])),
		Fuel:     math.Float32frombits(binary.LittleEndian.Uint32(data[12:16])),
		Gear:     int32(binary.LittleEndian.Uint32(data[16:20])),
		RPM:      int32(binary.LittleEndian.Uint32(data[20:24])),
		Steer:    math.Float32frombits(binary.LittleEndian.Uint32(data[24:28])),
		SpeedKMH: math.Float32frombits(binary.LittleEndian.Uint32(data[28:32])),
	}, nil
}

func (r *sharedMemoryReader) Close() error {
	if r == nil {
		return nil
	}

	var errs []error
	if r.addr != 0 {
		if err := windows.UnmapViewOfFile(r.addr); err != nil {
			errs = append(errs, err)
		}
		r.addr = 0
	}
	if r.handle != 0 {
		if err := windows.CloseHandle(r.handle); err != nil {
			errs = append(errs, err)
		}
		r.handle = 0
	}

	return errors.Join(errs...)
}
