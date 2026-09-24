package controller

// Messages describe customer actions without exposing channel configuration.
func imageTaskErrorMessage(code string) string {
	switch code {
	case "task_not_found":
		return "No image task with this ID is available to this API key. Check the task ID and use the key that submitted it."
	case "task_store_unavailable":
		return "Image task storage is temporarily unavailable. Retry querying the same task before submitting another generation."
	case "channel_unavailable":
		return "No compatible image channel is currently available for this model. Please retry later or select another model."
	case "invalid_request_id":
		return "X-Request-ID must be a valid request identifier. Use a unique identifier for each new generation and reuse it only for retries."
	case "request_id_conflict":
		return "This request ID is already associated with a different request. Reuse the original body to retry, or choose a new ID for a new generation."
	case "invalid_request":
		return "The request body must be valid JSON matching this model's input schema."
	case "invalid_image_count":
		return "The requested image count does not match the model's supported count rules. Check its input schema."
	case "internal_admin_token_required", "customer_token_required":
		return "This API key cannot use the requested image operation. Use a key with access to this endpoint."
	case "unsupported_image_execution":
		return "This model does not support asynchronous image tasks. Check its published endpoint in the model catalog."
	case "unsupported_image_adapter", "adapter_unavailable", "provider_contract_mismatch", "provider_binding_required", "provider_mapping_unavailable", "invalid_contract", "invalid_provider_binding", "reserved_contract_unavailable":
		return "This model's image route is currently unavailable. Contact support with the request ID or select another model."
	case "invalid_image_price", "measured_image_billing_not_supported_by_queue_adapter", "pricing_metadata_unavailable", "invalid_image_usage", "invalid_image_metering":
		return "Billing configuration for this image model is unavailable. Contact support with the request ID before retrying."
	case "balance_unavailable":
		return "Your account balance could not be checked. Please retry later."
	default:
		return "The image request could not be completed. Contact support with the request ID."
	}
}
