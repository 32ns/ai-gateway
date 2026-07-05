package web

func init() {
	translations[localeEN]["user_ip_concurrent_request_limit_override"] = "User same-IP concurrent request limit"
	translations[localeEN]["user_ip_concurrent_request_limit_override_hint"] = "Blank or 0 means no same-IP concurrency limit for this user."

	translations[localeZH]["user_ip_concurrent_request_limit_override"] = "\u5355\u7528\u6237\u540c IP \u5e76\u53d1\u8bf7\u6c42\u4e0a\u9650"
	translations[localeZH]["user_ip_concurrent_request_limit_override_hint"] = "\u7559\u7a7a\u6216 0 \u8868\u793a\u8be5\u7528\u6237\u4e0d\u9650\u5236\u540c IP \u5e76\u53d1\u3002"
}
