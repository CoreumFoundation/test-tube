use crate::runner::error::{DecodeError, RunnerError};
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use base64::Engine;
use cosmrs::proto::cosmos::base::abci::v1beta1::SimulationResponse;
use cosmrs::proto::cosmos::base::abci::v1beta1::{GasInfo, TxMsgData};
use cosmrs::rpc::endpoint::broadcast::tx_commit::Response as TxCommitResponse;
use cosmwasm_std::{Attribute, Event};
use prost::Message;
use std::ffi::CString;
use std::str::Utf8Error;

pub type RunnerResult<T> = Result<T, RunnerError>;
pub type RunnerExecuteResult<R> = Result<ExecuteResponse<R>, RunnerError>;

#[derive(Debug, Clone, PartialEq)]
pub struct ExecuteResponse<R>
where
    R: prost::Message + Default,
{
    pub data: R,
    pub raw_data: Vec<u8>,
    pub events: Vec<Event>,
    pub gas_info: GasInfo,
}

impl<R> TryFrom<SimulationResponse> for ExecuteResponse<R>
where
    R: prost::Message + Default,
{
    type Error = RunnerError;

    fn try_from(res: SimulationResponse) -> Result<Self, Self::Error> {
        let result = res.clone().result.unwrap();
        let msg_data = result
            .msg_responses
            // since this tx contains exactly 1 msg
            // when getting none of them, that means error
            .first()
            .ok_or(RunnerError::ExecuteError { msg: result.log })?;
        let data = R::decode(msg_data.value.as_slice()).map_err(DecodeError::ProtoDecodeError)?;

        let events = result
            .events
            .into_iter()
            .map(|e| -> Result<Event, DecodeError> {
                Ok(Event::new(e.r#type.to_string()).add_attributes(
                    e.attributes
                        .into_iter()
                        .map(|a| -> Result<Attribute, Utf8Error> {
                            Ok(Attribute {
                                key: std::str::from_utf8(&a.key).unwrap_or_default().to_string(),
                                value: std::str::from_utf8(&a.value)
                                    .unwrap_or_default()
                                    .to_string(),
                            })
                        })
                        .collect::<Result<Vec<Attribute>, Utf8Error>>()?,
                ))
            })
            .collect::<Result<Vec<Event>, DecodeError>>()?;

        let result = res.clone().result.unwrap();
        Ok(ExecuteResponse {
            data,
            #[allow(deprecated)]
            raw_data: result.clone().data,
            events,
            gas_info: GasInfo {
                gas_wanted: res.clone().gas_info.unwrap().gas_wanted as u64,
                gas_used: res.gas_info.unwrap().gas_used as u64,
            },
        })
    }
}

impl<R> TryFrom<TxCommitResponse> for ExecuteResponse<R>
where
    R: prost::Message + Default,
{
    type Error = RunnerError;

    fn try_from(tx_commit_response: TxCommitResponse) -> Result<Self, Self::Error> {
        let res = tx_commit_response.tx_result;
        let tx_msg_data =
            TxMsgData::decode(res.data.clone()).map_err(DecodeError::ProtoDecodeError)?;

        let msg_data = &tx_msg_data
            .msg_responses
            // since this tx contains exactly 1 msg
            // when getting none of them, that means error
            .first()
            .ok_or(RunnerError::ExecuteError {
                msg: res.log.to_string(),
            })?;

        let data = R::decode(msg_data.value.as_slice()).map_err(DecodeError::ProtoDecodeError)?;

        let events = res
            .events
            .into_iter()
            .map(|e| -> Result<Event, DecodeError> {
                Ok(Event::new(e.kind).add_attributes(
                    e.attributes
                        .into_iter()
                        .map(|a| -> Result<Attribute, Utf8Error> {
                            Ok(Attribute {
                                key: a.key.to_string(),
                                value: a.value.to_string(),
                            })
                        })
                        .collect::<Result<Vec<Attribute>, Utf8Error>>()?,
                ))
            })
            .collect::<Result<Vec<Event>, DecodeError>>()?;

        Ok(Self {
            data,
            raw_data: res.data.into(),
            events,
            gas_info: GasInfo {
                gas_wanted: res.gas_wanted as u64,
                gas_used: res.gas_used as u64,
            },
        })
    }
}

/// `RawResult` facilitates type conversions between Go and Rust,
///
/// Since Go struct could not be exposed via cgo due to limitations on
/// its unstable behavior of its memory layout.
/// So, apart from passing primitive types, we need to:
///
///   Go { T -> bytes(T) -> base64 -> *c_char }
///                      ↓
///   Rust { *c_char -> base64 -> bytes(T') -> T' }
///
/// Where T and T' are corresponding data structures, regardless of their encoding
/// in their respective language plus error information.
///
/// Resulted bytes are tagged by prepending 4 bytes to byte array
/// before base64 encoded. The prepended byte represents
///   0 -> Ok
///   1 -> QueryError
///   2 -> ExecuteError
///
/// The rest are undefined and remaining spaces are reserved for future use.
#[derive(Debug)]
pub struct RawResult(Result<Vec<u8>, RunnerError>);

impl RawResult {
    /// Convert ptr to AppResult. Check the first byte tag before decoding the rest of the bytes into expected type
    ///
    /// # Safety
    ///
    /// `ptr` must contain a valid pointer to a null-terminated C string with base64 encoded bytes.
    /// The decoded content must be a valid utf-8 string.
    pub unsafe fn from_ptr(ptr: *mut std::os::raw::c_char) -> Option<Self> {
        if ptr.is_null() {
            return None;
        }

        let c_string = unsafe { CString::from_raw(ptr) };
        let base64_bytes = c_string.to_bytes();
        let bytes = BASE64_STANDARD.decode(base64_bytes).unwrap();
        let code = bytes[0];
        let content = &bytes[1..];

        if code == 0 {
            Some(Self(Ok(content.to_vec())))
        } else {
            let content_string = CString::new(content)
                .unwrap()
                .to_str()
                .expect("Go code must encode valid UTF-8 string")
                .to_string();

            let error = match code {
                1 => RunnerError::QueryError {
                    msg: content_string,
                },
                2 => RunnerError::ExecuteError {
                    msg: content_string,
                },
                _ => panic!("undefined code: {}", code),
            };
            Some(Self(Err(error)))
        }
    }

    /// Convert ptr to AppResult. Use this function only when it is sure that the
    /// pointer is not a null pointer.
    ///
    /// # Safety
    /// There is a potential null pointer here, need to be extra careful before
    /// calling this function
    pub unsafe fn from_non_null_ptr(ptr: *mut std::os::raw::c_char) -> Self {
        Self::from_ptr(ptr).expect("Must ensure that the pointer is not null")
    }

    pub fn into_result(self) -> Result<Vec<u8>, RunnerError> {
        self.0
    }
}
