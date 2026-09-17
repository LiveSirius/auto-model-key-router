from __future__ import annotations

import hmac


VISITOR_API_KEY = "amkr-visitor"
VISITOR_MODEL_PREFIX = "amkr-"

try:
    import itsdangerous as _visitor_dependency
except ModuleNotFoundError:
    _visitor_dependency = None


# Python installers do not record which extras were requested. The dependency
# installed only by the visitor extra is therefore the runtime feature marker.
VISITOR_FEATURE_AVAILABLE = _visitor_dependency is not None


def visitor_feature_available() -> bool:
    return VISITOR_FEATURE_AVAILABLE


def is_visitor_api_key(api_key: str) -> bool:
    # 比较 bytes：hmac.compare_digest 对含非 ASCII 的 str 会抛 TypeError，而 HTTP
    # 头由 Starlette 按 latin-1 解码，攻击者塞非 ASCII 凭据就能把它变成 500。
    return VISITOR_FEATURE_AVAILABLE and hmac.compare_digest(
        api_key.encode("utf-8"), VISITOR_API_KEY.encode("utf-8")
    )
