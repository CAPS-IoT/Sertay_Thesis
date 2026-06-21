#[link(wasm_import_module = "env")]
extern "C" {
    fn log_message(msg: *const u8, len: u32);
}

fn log(msg: &str) {
    unsafe {
        log_message(msg.as_ptr(), msg.len() as u32);
    }
}

#[no_mangle]
pub extern "C" fn process_event() -> i32 {
    log("basic-edge-demo: running deterministic no-input computation");

    let mut checksum = 0_u32;
    for value in 1_u32..=32 {
        checksum = checksum.wrapping_add(value.wrapping_mul(value));
    }

    if checksum == 11_440 {
        log("basic-edge-demo: computation completed");
        0
    } else {
        log("basic-edge-demo: computation failed");
        -1
    }
}
