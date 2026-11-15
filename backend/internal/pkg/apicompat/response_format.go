package apicompat

import "encoding/json"

func anthropicOutputFormatToResponsesTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	obj, ok := rawJSONObject(raw)
	if !ok || rawString(obj["type"]) != "json_schema" || len(normalizedRawJSON(obj["schema"])) == 0 {
		return nil, unsupportedRequestConversion("output_config.format requires a json_schema with schema for OpenAI conversion")
	}
	// Responses requires a schema name; Anthropic does not provide one. The
	// schema itself and any caller-provided additional options stay intact.
	if rawString(obj["name"]) == "" {
		obj["name"] = rawJSONString("response")
	}
	return json.Marshal(obj)
}

func responsesTextFormatToAnthropicOutputFormat(raw json.RawMessage) (json.RawMessage, error) {
	obj, ok := rawJSONObject(raw)
	if !ok {
		return nil, unsupportedRequestConversion("text.format must be an object for Messages conversion")
	}
	switch rawString(obj["type"]) {
	case "text", "":
		return nil, nil
	case "json_schema":
		if len(normalizedRawJSON(obj["schema"])) == 0 {
			return nil, unsupportedRequestConversion("text.format.json_schema requires a schema for Messages conversion")
		}
		return json.Marshal(map[string]json.RawMessage{"type": rawJSONString("json_schema"), "schema": obj["schema"]})
	default:
		return nil, unsupportedRequestConversion("text.format type %q cannot be converted to Messages; provide an explicit json_schema", rawString(obj["type"]))
	}
}

func chatResponseFormatToResponsesTextFormat(raw json.RawMessage) json.RawMessage {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil
	}

	obj, ok := rawJSONObject(raw)
	if !ok || rawString(obj["type"]) != "json_schema" {
		return raw
	}

	schemaRaw := normalizedRawJSON(obj["json_schema"])
	if len(schemaRaw) == 0 {
		return raw
	}

	var schema map[string]json.RawMessage
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return raw
	}
	schema["type"] = rawJSONString("json_schema")

	out, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	return out
}

func responsesTextFormatToChatResponseFormat(raw json.RawMessage) json.RawMessage {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil
	}

	obj, ok := rawJSONObject(raw)
	if !ok || rawString(obj["type"]) != "json_schema" {
		return raw
	}
	if _, alreadyChatShape := obj["json_schema"]; alreadyChatShape {
		return raw
	}

	schema := make(map[string]json.RawMessage, len(obj))
	for key, value := range obj {
		if key == "type" {
			continue
		}
		schema[key] = value
	}
	if len(schema) == 0 {
		return raw
	}

	schemaRaw, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	out, err := json.Marshal(map[string]json.RawMessage{
		"type":        rawJSONString("json_schema"),
		"json_schema": schemaRaw,
	})
	if err != nil {
		return raw
	}
	return out
}

func normalizedRawJSON(raw json.RawMessage) json.RawMessage {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func rawJSONString(value string) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
