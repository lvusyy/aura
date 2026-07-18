//! 统一响应信封 {ok, data, error}，对齐 coding spec 的 E_ 错误码约定。

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

use crate::types::CapError;

/// 机器可读错误对象。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema, PartialEq, Eq)]
pub struct ErrObj {
    /// E_ 前缀机器码
    pub code: String,
    /// 英文错误描述
    pub message: String,
}

impl From<CapError> for ErrObj {
    fn from(e: CapError) -> Self {
        ErrObj {
            code: e.code().to_string(),
            message: e.to_string(),
        }
    }
}

/// 统一响应信封：ok 为真时 data 有值，否则 error 有值。
#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct Envelope<T> {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<T>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<ErrObj>,
}

impl<T> Envelope<T> {
    /// 成功信封。
    pub fn ok(data: T) -> Self {
        Envelope {
            ok: true,
            data: Some(data),
            error: None,
        }
    }

    /// 失败信封。
    pub fn error(err: ErrObj) -> Self {
        Envelope {
            ok: false,
            data: None,
            error: Some(err),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn envelope_ok_serializes() {
        let e = Envelope::ok(42i32);
        let v = serde_json::to_value(&e).unwrap();
        assert_eq!(v["ok"], true);
        assert_eq!(v["data"], 42);
        assert!(v.get("error").is_none());
    }

    #[test]
    fn envelope_error_serializes() {
        let e: Envelope<i32> = Envelope::error(ErrObj {
            code: "E_INTERNAL".into(),
            message: "boom".into(),
        });
        let v = serde_json::to_value(&e).unwrap();
        assert_eq!(v["ok"], false);
        assert_eq!(v["error"]["code"], "E_INTERNAL");
        assert!(v.get("data").is_none());
    }
}
