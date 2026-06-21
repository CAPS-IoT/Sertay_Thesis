package main

import (
	"log"

	wasmtime "github.com/bytecodealliance/wasmtime-go/v29"
)

// registerHAL registers the Edge HAL host functions into the linker.
// These are the same three functions the ESP32/WAMR host provides,
// but backed by HTTP payload data instead of real hardware.
//
// Wasm guest imports (module "env"):
//   get_temperature() -> f32
//   get_humidity()    -> f32
//   log_message(ptr: i32, len: i32)
func registerHAL(linker *wasmtime.Linker) {
	must(linker.FuncWrap("env", "get_temperature",
		func() float32 {
			log.Printf("[HAL] get_temperature() -> %.2f", sensorData.Temperature)
			return sensorData.Temperature
		},
	))

	must(linker.FuncWrap("env", "get_humidity",
		func() float32 {
			log.Printf("[HAL] get_humidity() -> %.2f", sensorData.Humidity)
			return sensorData.Humidity
		},
	))

	must(linker.FuncWrap("env", "log_message",
		func(caller *wasmtime.Caller, ptr int32, length int32) {
			mem := caller.GetExport("memory")
			if mem == nil {
				log.Println("[HAL] log_message: no memory export")
				return
			}
			// The guest passes a pointer/length pair in linear memory; validate the
			// range before copying it into Go-owned memory for logging.
			data := mem.Memory().UnsafeData(caller)
			if int(ptr)+int(length) > len(data) {
				log.Printf("[HAL] log_message: out of bounds (ptr=%d, len=%d, mem=%d)",
					ptr, length, len(data))
				return
			}
			msg := make([]byte, length)
			copy(msg, data[ptr:int(ptr)+int(length)])
			log.Printf("[WasmGuest] %s", string(msg))
		},
	))
}

func must(err error) {
	if err != nil {
		log.Fatalf("Failed to register HAL function: %v", err)
	}
}
