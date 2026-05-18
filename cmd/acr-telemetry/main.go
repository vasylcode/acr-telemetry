//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	physicsMapName = `Local\acpmf_physics`
	physicsMapSize = 800
	pollInterval   = 5 * time.Millisecond
	streamInterval = 16 * time.Millisecond
)

var procOpenFileMappingW = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileMappingW")

type physicsSample struct {
	PacketID      int32
	Gas           float32
	Brake         float32
	Fuel          float32
	Gear          int32
	RPM           int32
	CurrentMaxRPM int32
	Steer         float32
	SpeedKMH      float32
}

type dashState struct {
	PacketID             int32   `json:"packetId"`
	SpeedKMH             float32 `json:"speedKmh"`
	PhysicsRPM           int32   `json:"physicsRpm"`
	PhysicsCurrentMaxRPM int32   `json:"physicsCurrentMaxRpm"`
	RPMRatio             float32 `json:"rpmRatio"`
	Gear                 string  `json:"gear"`
	Gas                  float32 `json:"gas"`
	Brake                float32 `json:"brake"`
	Steer                float32 `json:"steer"`
}

var latestState atomic.Value

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "acr-telemetry: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8765", "HTTP listen address")
	flag.Parse()

	ctxDone := make(chan os.Signal, 1)
	signal.Notify(ctxDone, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ctxDone)

	fmt.Println("ACR telemetry dashboard")
	fmt.Println("Waiting for Assetto Corsa Rally physics map...")

	reader, err := waitForSharedMemory(ctxDone)
	if err != nil {
		return err
	}
	defer reader.Close()

	server := &http.Server{Addr: *addr, Handler: routes()}
	go func() {
		fmt.Printf("Dashboard: http://localhost%s\n", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "web server: %v\n", err)
		}
	}()

	fmt.Printf("Connected to %s (%d bytes). Press Ctrl+C to stop.\n\n", physicsMapName, physicsMapSize)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	lastPacketID := int32(-1)
	for {
		select {
		case <-ctxDone:
			fmt.Println("Stopping.")
			_ = server.Close()
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
			storeDashState(sample.toDashState())
		}
	}
}

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", dashboardHandler)
	mux.HandleFunc("/events", eventsHandler)
	mux.HandleFunc("/api/state", stateHandler)
	return mux
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(streamInterval)
	defer ticker.Stop()

	lastPacketID := int32(-1)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			state := loadDashState()
			if state.PacketID == lastPacketID {
				continue
			}
			lastPacketID = state.PacketID
			writeDashStateJSON(w, "data: ", state, "\n\n")
			flusher.Flush()
		}
	}
}

func stateHandler(w http.ResponseWriter, _ *http.Request) {
	state := loadDashState()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeDashStateJSON(w, "", state, "\n")
}

func writeDashStateJSON(w http.ResponseWriter, prefix string, state dashState, suffix string) {
	fmt.Fprintf(w, "%s{\"packetId\":%d,\"speedKmh\":%.1f,\"physics\":{\"rpm\":%d,\"currentMaxRpm\":%d},\"rpmRatio\":%.4f,\"gear\":%q,\"gas\":%.3f,\"brake\":%.3f,\"steer\":%.3f}%s",
		prefix, state.PacketID, state.SpeedKMH, state.PhysicsRPM, state.PhysicsCurrentMaxRPM, state.RPMRatio, state.Gear, state.Gas, state.Brake, state.Steer, suffix)
}

func (s physicsSample) toDashState() dashState {
	rpmRatio := float32(0)
	if s.CurrentMaxRPM > 0 {
		rpmRatio = float32(s.RPM) / float32(s.CurrentMaxRPM)
	}
	return dashState{
		PacketID:             s.PacketID,
		SpeedKMH:             s.SpeedKMH,
		PhysicsRPM:           s.RPM,
		PhysicsCurrentMaxRPM: s.CurrentMaxRPM,
		RPMRatio:             rpmRatio,
		Gear:                 formatGear(s.Gear),
		Gas:                  s.Gas,
		Brake:                s.Brake,
		Steer:                s.Steer,
	}
}

func storeDashState(state dashState) {
	latestState.Store(state)
}

func loadDashState() dashState {
	state, ok := latestState.Load().(dashState)
	if !ok {
		return dashState{Gear: "N"}
	}
	return state
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
	if r.size < 596 {
		return physicsSample{}, fmt.Errorf("physics map too small: %d bytes", r.size)
	}

	data := unsafe.Slice((*byte)(unsafe.Pointer(r.addr)), r.size)
	return physicsSample{
		PacketID:      int32(binary.LittleEndian.Uint32(data[0:4])),
		Gas:           math.Float32frombits(binary.LittleEndian.Uint32(data[4:8])),
		Brake:         math.Float32frombits(binary.LittleEndian.Uint32(data[8:12])),
		Fuel:          math.Float32frombits(binary.LittleEndian.Uint32(data[12:16])),
		Gear:          int32(binary.LittleEndian.Uint32(data[16:20])),
		RPM:           int32(binary.LittleEndian.Uint32(data[20:24])),
		Steer:         math.Float32frombits(binary.LittleEndian.Uint32(data[24:28])),
		SpeedKMH:      math.Float32frombits(binary.LittleEndian.Uint32(data[28:32])),
		CurrentMaxRPM: int32(binary.LittleEndian.Uint32(data[588:592])),
	}, nil
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>ACR Telemetry Debug</title>
<style>
:root{color-scheme:dark;--bg:#0b0d10;--panel:#14181d;--line:#2a3037;--text:#e8edf2;--muted:#8d98a5;--live:#29ff6a;--stale:#ffb020}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:16px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace;padding:24px}.wrap{max-width:760px;margin:0 auto}h1{font-size:22px;margin:0 0 8px}.meta{color:var(--muted);margin-bottom:20px}.status{display:inline-flex;gap:8px;align-items:center}.dot{width:10px;height:10px;border-radius:50%;background:var(--stale)}.dot.live{background:var(--live);box-shadow:0 0 12px var(--live)}table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line)}th,td{padding:10px 12px;border-bottom:1px solid var(--line);text-align:left}th{width:42%;color:var(--muted);font-weight:600}td{font-variant-numeric:tabular-nums}pre{white-space:pre-wrap;background:#07090b;border:1px solid var(--line);padding:12px;overflow:auto}
</style>
</head>
<body>
<main class="wrap">
  <h1>ACR Telemetry Debug</h1>
  <div class="meta"><span class="status"><span class="dot" id="dot"></span><span id="status">connecting</span></span></div>
  <table><tbody id="rows"></tbody></table>
  <h2>Raw JSON</h2>
  <pre id="raw">{}</pre>
</main>
<script>
const els={rows:document.getElementById('rows'),raw:document.getElementById('raw'),dot:document.getElementById('dot'),status:document.getElementById('status')};
let state={speedKmh:0,physics:{rpm:0,currentMaxRpm:0},rpmRatio:0,gear:'N',gas:0,brake:0,steer:0},lastSeen=0;
const fields=[['packetId','Packet ID'],['speedKmh','Speed km/h'],['physics.rpm','Physics RPM'],['physics.currentMaxRpm','Physics current max RPM'],['rpmRatio','RPM ratio'],['gear','Gear'],['gas','Throttle'],['brake','Brake'],['steer','Steer']];
els.rows.innerHTML=fields.map(([key,label])=>'<tr><th>'+label+'</th><td id="f_'+key+'">-</td></tr>').join('');
function valueAt(obj,path){return path.split('.').reduce((v,k)=>v?.[k],obj)}
function render(){
  for(const [key] of fields){const el=document.getElementById('f_'+key);let value=valueAt(state,key);if(typeof value==='number')value=Number.isInteger(value)?value:value.toFixed(4);el.textContent=value ?? '-'}
  els.raw.textContent=JSON.stringify(state,null,2);
  const live=Date.now()-lastSeen<750; els.dot.classList.toggle('live',live); els.status.textContent=live?'live':'waiting';
  requestAnimationFrame(render);
}
requestAnimationFrame(render);
const es=new EventSource('/events');
es.onmessage=e=>{state=JSON.parse(e.data);lastSeen=Date.now()};
es.onerror=()=>{els.status.textContent='reconnecting'};
</script>
</body>
</html>`

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
