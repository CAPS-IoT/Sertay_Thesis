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
//
//	get_resource_f32(resource_ptr: i32, resource_len: i32, key_ptr: i32, key_len: i32) -> f32
//	get_resource_i32(resource_ptr: i32, resource_len: i32, key_ptr: i32, key_len: i32) -> i32
//	get_resource_bool(resource_ptr: i32, resource_len: i32, key_ptr: i32, key_len: i32) -> i32
//	set_output_i32(key_ptr: i32, key_len: i32, value: i32)
//	set_output_f32(key_ptr: i32, key_len: i32, value: f32)
//	get_temperature() -> f32
//	get_humidity()    -> f32
//	log_message(ptr: i32, len: i32)
func registerHAL(linker *wasmtime.Linker) {
	must(linker.FuncWrap("env", "get_resource_f32",
		func(caller *wasmtime.Caller, resourcePtr int32, resourceLen int32, keyPtr int32, keyLen int32) float32 {
			resource, ok := wasmString(caller, resourcePtr, resourceLen)
			if !ok {
				log.Printf("[HAL] get_resource_f32: invalid resource pointer (ptr=%d, len=%d)", resourcePtr, resourceLen)
				return -999.0
			}
			key, ok := wasmString(caller, keyPtr, keyLen)
			if !ok {
				log.Printf("[HAL] get_resource_f32: invalid key pointer (ptr=%d, len=%d)", keyPtr, keyLen)
				return -999.0
			}
			value, ok := resourceF32(resource, key)
			if !ok {
				log.Printf("[HAL] get_resource_f32(%s.%s) missing", resource, key)
				return -999.0
			}
			log.Printf("[HAL] get_resource_f32(%s.%s) -> %.2f", resource, key, value)
			return value
		},
	))

	must(linker.FuncWrap("env", "get_resource_i32",
		func(caller *wasmtime.Caller, resourcePtr int32, resourceLen int32, keyPtr int32, keyLen int32) int32 {
			resource, ok := wasmString(caller, resourcePtr, resourceLen)
			if !ok {
				log.Printf("[HAL] get_resource_i32: invalid resource pointer (ptr=%d, len=%d)", resourcePtr, resourceLen)
				return -999
			}
			key, ok := wasmString(caller, keyPtr, keyLen)
			if !ok {
				log.Printf("[HAL] get_resource_i32: invalid key pointer (ptr=%d, len=%d)", keyPtr, keyLen)
				return -999
			}
			value, ok := resourceI32(resource, key)
			if !ok {
				log.Printf("[HAL] get_resource_i32(%s.%s) missing", resource, key)
				return -999
			}
			log.Printf("[HAL] get_resource_i32(%s.%s) -> %d", resource, key, value)
			return value
		},
	))

	must(linker.FuncWrap("env", "get_resource_bool",
		func(caller *wasmtime.Caller, resourcePtr int32, resourceLen int32, keyPtr int32, keyLen int32) int32 {
			resource, ok := wasmString(caller, resourcePtr, resourceLen)
			if !ok {
				log.Printf("[HAL] get_resource_bool: invalid resource pointer (ptr=%d, len=%d)", resourcePtr, resourceLen)
				return 0
			}
			key, ok := wasmString(caller, keyPtr, keyLen)
			if !ok {
				log.Printf("[HAL] get_resource_bool: invalid key pointer (ptr=%d, len=%d)", keyPtr, keyLen)
				return 0
			}
			value, ok := resourceBool(resource, key)
			if !ok {
				log.Printf("[HAL] get_resource_bool(%s.%s) missing", resource, key)
				return 0
			}
			log.Printf("[HAL] get_resource_bool(%s.%s) -> %t", resource, key, value)
			if value {
				return 1
			}
			return 0
		},
	))

	must(linker.FuncWrap("env", "set_output_i32",
		func(caller *wasmtime.Caller, keyPtr int32, keyLen int32, value int32) {
			key, ok := wasmString(caller, keyPtr, keyLen)
			if !ok {
				log.Printf("[HAL] set_output_i32: invalid key pointer (ptr=%d, len=%d)", keyPtr, keyLen)
				return
			}
			setOutputF32(key, float32(value))
			log.Printf("[HAL] set_output_i32(%s) -> %d", key, value)
		},
	))

	must(linker.FuncWrap("env", "set_output_f32",
		func(caller *wasmtime.Caller, keyPtr int32, keyLen int32, value float32) {
			key, ok := wasmString(caller, keyPtr, keyLen)
			if !ok {
				log.Printf("[HAL] set_output_f32: invalid key pointer (ptr=%d, len=%d)", keyPtr, keyLen)
				return
			}
			setOutputF32(key, value)
			log.Printf("[HAL] set_output_f32(%s) -> %.2f", key, value)
		},
	))

	must(linker.FuncWrap("env", "get_temperature",
		func() float32 {
			value, ok := resourceF32("DHT", "temperature")
			if !ok {
				log.Printf("[HAL] get_temperature() missing")
				return -999.0
			}
			log.Printf("[HAL] get_temperature() -> %.2f", value)
			return value
		},
	))

	must(linker.FuncWrap("env", "get_humidity",
		func() float32 {
			value, ok := resourceF32("DHT", "humidity")
			if !ok {
				log.Printf("[HAL] get_humidity() missing")
				return -999.0
			}
			log.Printf("[HAL] get_humidity() -> %.2f", value)
			return value
		},
	))

	must(linker.FuncWrap("env", "log_message",
		func(caller *wasmtime.Caller, ptr int32, length int32) {
			data, ok := wasmBytes(caller, ptr, length)
			if !ok {
				log.Printf("[HAL] log_message: out of bounds (ptr=%d, len=%d, mem=%d)",
					ptr, length, wasmMemoryLen(caller))
				return
			}
			msg := make([]byte, len(data))
			copy(msg, data)
			log.Printf("[WasmGuest] %s", string(msg))
		},
	))
}

func resourceF32(resource string, key string) (float32, bool) {
	value, ok := resourceInputF32(&sensorData, resource, key)
	return value, ok
}

func resourceI32(resource string, key string) (int32, bool) {
	value, ok := resourceInputI32(&sensorData, resource, key)
	return value, ok
}

func resourceBool(resource string, key string) (bool, bool) {
	value, ok := resourceInputBool(&sensorData, resource, key)
	return value, ok
}

func setOutputF32(key string, value float32) {
	if outputData == nil {
		outputData = make(map[string]float32)
	}
	outputData[key] = value
}

func wasmString(caller *wasmtime.Caller, ptr int32, length int32) (string, bool) {
	data, ok := wasmBytes(caller, ptr, length)
	if !ok {
		return "", false
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	return string(buf), true
}

func wasmBytes(caller *wasmtime.Caller, ptr int32, length int32) ([]byte, bool) {
	if ptr < 0 || length < 0 {
		return nil, false
	}
	mem := caller.GetExport("memory")
	if mem == nil {
		log.Println("[HAL] memory export missing")
		return nil, false
	}
	data := mem.Memory().UnsafeData(caller)
	start := int64(ptr)
	end := start + int64(length)
	if start < 0 || end < start || end > int64(len(data)) {
		return nil, false
	}
	return data[int(start):int(end)], true
}

func wasmMemoryLen(caller *wasmtime.Caller) int {
	mem := caller.GetExport("memory")
	if mem == nil {
		return 0
	}
	return len(mem.Memory().UnsafeData(caller))
}

func must(err error) {
	if err != nil {
		log.Fatalf("Failed to register HAL function: %v", err)
	}
}
