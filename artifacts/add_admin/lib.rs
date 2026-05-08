use std::{slice, str};

use rubixwasm_std::errors::WasmError;
use serde::{Deserialize, Serialize};
use rubixwasm_std::contract_fn;

extern "C" {
    // write_to_json_file writes data to a JSON file
    pub fn write_to_json_file(
        url_ptr: *const u8,
        url_len: usize,
        resp_ptr_ptr: *mut *const u8,
        resp_len_ptr: *mut usize,
    ) -> i32;
}


#[derive(Serialize, Deserialize)]
pub struct AddAdminReq {
    pub admin_did: String,
}

// call_do_api_call is helper function for do_api_call import function 
pub fn call_write_to_file(input_data: &AddAdminReq) -> Result<String, WasmError> {
    unsafe {
        // Convert the input data to bytes
        let input_bytes = serde_json::to_string(&input_data).unwrap().into_bytes();
        let input_ptr = input_bytes.as_ptr();
        let input_len = input_bytes.len();

        // Allocate space for the response pointer and length
        let mut resp_ptr: *const u8 = std::ptr::null();
        let mut resp_len: usize = 0;

        // Call the imported host function
        let result = write_to_json_file(
            input_ptr,
            input_len,
            &mut resp_ptr,
            &mut resp_len,
        );
        
        if result != 0 {
            return Err(WasmError::from(format!("Host function returned error code {}", result)));
        }

        // Ensure the response pointer is not null
        if resp_ptr.is_null() {
            return Err(WasmError::from("Response pointer is null".to_string()));
        }

        // Convert the response back to a Rust String
        let response_slice = slice::from_raw_parts(resp_ptr, resp_len);
        match str::from_utf8(response_slice) {
            Ok(s) => Ok(s.to_string()),
            Err(_) => Err(WasmError::from("Invalid UTF-8 response".to_string())),
        }
    }
}


#[contract_fn]
pub fn add_admin(inp: AddAdminReq) -> Result<String, WasmError> {
    
    match call_write_to_file(&inp) {
        Ok(resp) => {
            Ok(resp)
        },
        Err(e) => {
            Err(e)
        }
    }
}