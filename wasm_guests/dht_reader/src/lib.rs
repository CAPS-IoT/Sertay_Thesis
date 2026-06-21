#[link(wasm_import_module = "env")]
extern "C" {
    // Both ESP32/WAMR and edge/wasmtime hosts provide this same HAL surface.
    fn get_temperature() -> f32;
    fn get_humidity() -> f32;
    fn log_message(msg: *const u8, len: u32);
}

fn log(msg: &str) {
    unsafe {
        log_message(msg.as_ptr(), msg.len() as u32);
    }
}

#[no_mangle]
pub extern "C" fn process_event() -> i32 {
    log("Wasm guest: reading DHT sensor...");

    let temp = unsafe { get_temperature() };
    let hum = unsafe { get_humidity() };

    // -999.0 is the error sentinel returned by the host API.
    if temp < -998.0 || hum < -998.0 {
        log("Wasm guest: sensor read failed!");
        return -1;
    }

    log("Wasm guest: sensor read successful v3");
    0
}
