#![no_std]

#[cfg(not(test))]
use core::panic::PanicInfo;

#[link(wasm_import_module = "env")]
extern "C" {
    fn get_resource_f32(
        resource: *const u8,
        resource_len: u32,
        key: *const u8,
        key_len: u32,
    ) -> f32;
    fn get_resource_i32(
        resource: *const u8,
        resource_len: u32,
        key: *const u8,
        key_len: u32,
    ) -> i32;
    fn get_resource_bool(
        resource: *const u8,
        resource_len: u32,
        key: *const u8,
        key_len: u32,
    ) -> i32;
    fn set_output_i32(key: *const u8, key_len: u32, value: i32);
    fn set_output_f32(key: *const u8, key_len: u32, value: f32);
    fn log_message(msg: *const u8, len: u32);
}

const MISSING: f32 = -998.0;
const MISSING_I32: i32 = -998;
const DEMO_TEMPERATURE_OVERRIDE_C: f32 = 30.0;

#[cfg(not(test))]
#[panic_handler]
fn panic(_info: &PanicInfo) -> ! {
    loop {}
}

fn log(msg: &str) {
    unsafe {
        log_message(msg.as_ptr(), msg.len() as u32);
    }
}

fn resource_f32(resource: &str, key: &str) -> f32 {
    unsafe {
        get_resource_f32(
            resource.as_ptr(),
            resource.len() as u32,
            key.as_ptr(),
            key.len() as u32,
        )
    }
}

fn resource_i32(resource: &str, key: &str) -> i32 {
    unsafe {
        get_resource_i32(
            resource.as_ptr(),
            resource.len() as u32,
            key.as_ptr(),
            key.len() as u32,
        )
    }
}

fn resource_bool(resource: &str, key: &str) -> bool {
    unsafe {
        get_resource_bool(
            resource.as_ptr(),
            resource.len() as u32,
            key.as_ptr(),
            key.len() as u32,
        ) != 0
    }
}

fn output_i32(key: &str, value: i32) {
    unsafe {
        set_output_i32(key.as_ptr(), key.len() as u32, value);
    }
}

fn output_f32(key: &str, value: f32) {
    unsafe {
        set_output_f32(key.as_ptr(), key.len() as u32, value);
    }
}

fn with_default(value: f32, default_value: f32, name: &str) -> f32 {
    if value < MISSING {
        log(name);
        default_value
    } else {
        value
    }
}

fn heat_index_c(temp_c: f32, humidity: f32) -> f32 {
    let temp_f = temp_c * 9.0 / 5.0 + 32.0;
    let relative_humidity = humidity.clamp(0.0, 100.0);

    // Follow the NWS two-stage calculation. The Rothfusz regression is only
    // valid for hot conditions; applying it directly at low temperatures can
    // produce a heat index far above the actual temperature.
    let simple_hi_f = 0.5
        * (temp_f
            + 61.0
            + (temp_f - 68.0) * 1.2
            + relative_humidity * 0.094);
    let averaged_hi_f = 0.5 * (simple_hi_f + temp_f);
    if averaged_hi_f < 80.0 {
        return (averaged_hi_f - 32.0) * 5.0 / 9.0;
    }

    let mut hi_f = -42.379 + 2.049_015_3 * temp_f + 10.143_331 * relative_humidity
        - 0.224_755_4 * temp_f * relative_humidity
        - 0.006_837_83 * temp_f * temp_f
        - 0.054_817_17 * relative_humidity * relative_humidity
        + 0.001_228_74 * temp_f * temp_f * relative_humidity
        + 0.000_852_82 * temp_f * relative_humidity * relative_humidity
        - 0.000_001_99 * temp_f * temp_f * relative_humidity * relative_humidity;

    if relative_humidity < 13.0 && (80.0..=112.0).contains(&temp_f) {
        let radicand = (17.0 - (temp_f - 95.0).abs()) / 17.0;
        let mut root = if radicand > 1.0 { radicand } else { 1.0 };
        for _ in 0..6 {
            root = 0.5 * (root + radicand / root);
        }
        hi_f -= ((13.0 - relative_humidity) / 4.0) * root;
    } else if relative_humidity > 85.0 && (80.0..=87.0).contains(&temp_f) {
        hi_f += ((relative_humidity - 85.0) / 10.0) * ((87.0 - temp_f) / 5.0);
    }

    (hi_f - 32.0) * 5.0 / 9.0
}

fn comfort_score(heat_index: f32, humidity: f32, lux: f32, occupied: bool, battery: f32) -> i32 {
    let mut score = 0;
    if heat_index >= 32.0 {
        score += 45;
    } else if heat_index >= 28.0 {
        score += 30;
    } else if heat_index <= 16.0 {
        score += 20;
    }
    if humidity >= 70.0 {
        score += 25;
    } else if humidity >= 60.0 {
        score += 12;
    }
    if occupied {
        score += 15;
    }
    if lux < 80.0 {
        score += 5;
    }
    if battery < 25.0 {
        score -= 15;
    }
    if score < 0 {
        0
    } else if score > 100 {
        100
    } else {
        score
    }
}

fn next_sample_seconds(score: i32, battery: f32) -> i32 {
    if battery < 20.0 {
        60
    } else if score >= 70 {
        5
    } else if score >= 40 {
        15
    } else {
        30
    }
}

fn actuator_command(score: i32, occupied: bool, battery: f32, heat_index: f32) -> i32 {
    if battery < 15.0 {
        0
    } else if heat_index >= 38.0 {
        2
    } else if heat_index >= 28.0 && occupied {
        1
    } else if score >= 70 && occupied {
        1
    } else if score >= 85 {
        2
    } else {
        0
    }
}

#[no_mangle]
pub extern "C" fn process_event() -> i32 {
    log("hybrid-resource-demo: reading SIF resource inputs");

    let battery = resource_i32("BATTERY", "percent");
    let voltage = resource_i32("BATTERY", "voltageMv");
    let measured_temp = resource_f32("DHT", "temperature");
    let humidity = resource_f32("DHT", "humidity");
    let lux = with_default(
        resource_f32("LIGHT", "lux"),
        120.0,
        "hybrid-resource-demo: LIGHT.lux missing, using default 120",
    );
    let distance = with_default(
        resource_f32("OCCUPANCY", "distanceCm"),
        85.0,
        "hybrid-resource-demo: OCCUPANCY.distanceCm missing, using default 85",
    );
    let button = resource_bool("GPIO", "buttonPressed");

    if battery < MISSING_I32 || voltage < MISSING_I32 || measured_temp < MISSING || humidity < MISSING {
        log("hybrid-resource-demo: required BATTERY or DHT input missing");
        return -1;
    }

    let temp = if DEMO_TEMPERATURE_OVERRIDE_C >= 0.0 {
        log("hybrid-resource-demo: using Wasm demo temperature override");
        DEMO_TEMPERATURE_OVERRIDE_C
    } else {
        measured_temp
    };
    let battery_f = battery as f32;
    let occupied = distance < 150.0 || button;
    let temp_f = temp * 9.0 / 5.0 + 32.0;
    let heat_index = heat_index_c(temp, humidity);
    let score = comfort_score(heat_index, humidity, lux, occupied, battery_f);
    let sample = next_sample_seconds(score, battery_f);
    let actuator = actuator_command(score, occupied, battery_f, heat_index);

    output_f32("temperatureF", temp_f);
    output_f32("heatIndexC", heat_index);
    output_i32("comfortScore", score);
    output_i32("occupied", if occupied { 1 } else { 0 });
    output_i32("nextSampleSeconds", sample);
    output_i32("actuatorCommand", actuator);

    log("hybrid-resource-demo: battery/temp/humidity/lux/occupancy/button evaluated");
    if actuator == 1 {
        log("hybrid-resource-demo: risk=hot_humid_occupied actuator=green");
    } else if actuator == 2 {
        log("hybrid-resource-demo: risk=high_heat_index actuator=red");
    } else {
        log("hybrid-resource-demo: risk=normal actuator=none");
    }

    0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn assert_near(actual: f32, expected: f32) {
        assert!((actual - expected).abs() < 0.05, "{actual} != {expected}");
    }

    #[test]
    fn heat_index_uses_preliminary_formula_outside_hot_conditions() {
        assert_near(heat_index_c(10.0, 50.0), 9.18);
    }

    #[test]
    fn thirty_celsius_selects_green_actuator() {
        let heat_index = heat_index_c(30.0, 50.0);
        assert_near(heat_index, 31.05);
        let score = comfort_score(heat_index, 50.0, 120.0, true, 92.0);
        assert_eq!(score, 45);
        assert_eq!(actuator_command(score, true, 92.0, heat_index), 1);
    }
}
