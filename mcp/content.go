package mcp

// Annotations carries optional metadata about audience, priority, and modification time,
// shared by every content type (text, image, audio, resource link, embedded resource) and
// by Resource itself.
type Annotations struct {
	Audience     []string `json:"audience,omitempty"`
	Priority     *float64 `json:"priority,omitempty"`
	LastModified string   `json:"lastModified,omitempty"`
}

// EmbeddedResourceContents is the nested "resource" object inside a Content of type
// "resource". Exactly one of Text or Blob is normally set.
type EmbeddedResourceContents struct {
	URI         string       `json:"uri"`
	MimeType    string       `json:"mimeType,omitempty"`
	Text        string       `json:"text,omitempty"`
	Blob        string       `json:"blob,omitempty"`
	Annotations *Annotations `json:"annotations,omitempty"`
}

// Content is the tool/prompt result content union (text, image, audio, resource_link,
// resource), represented as one flat struct with per-type fields left empty as
// appropriate — Go has no native discriminated union, and a flat struct with omitempty
// fields produces exactly the wire shape each variant needs. Use the constructors below
// rather than building a Content literal directly.
type Content struct {
	Type        string                    `json:"type"`
	Text        string                    `json:"text,omitempty"`
	Data        string                    `json:"data,omitempty"`
	MimeType    string                    `json:"mimeType,omitempty"`
	URI         string                    `json:"uri,omitempty"`
	Name        string                    `json:"name,omitempty"`
	Description string                    `json:"description,omitempty"`
	Resource    *EmbeddedResourceContents `json:"resource,omitempty"`
	Annotations *Annotations              `json:"annotations,omitempty"`
	Meta        map[string]interface{}    `json:"_meta,omitempty"`
}

// Text builds a TextContent block.
func Text(text string) Content {
	return Content{Type: "text", Text: text}
}

// Image builds an ImageContent block. data is base64-encoded image bytes.
func Image(data, mimeType string) Content {
	return Content{Type: "image", Data: data, MimeType: mimeType}
}

// Audio builds an AudioContent block. data is base64-encoded audio bytes.
func Audio(data, mimeType string) Content {
	return Content{Type: "audio", Data: data, MimeType: mimeType}
}

// ResourceLinkContent builds a resource_link block: a pointer to a resource the client
// may separately fetch via resources/read, rather than embedding its content inline.
func ResourceLinkContent(uri, name, description, mimeType string) Content {
	return Content{Type: "resource_link", URI: uri, Name: name, Description: description, MimeType: mimeType}
}

// EmbeddedResourceContent builds a "resource" content block embedding a resource's
// contents directly, rather than only linking to it.
func EmbeddedResourceContent(r EmbeddedResourceContents) Content {
	return Content{Type: "resource", Resource: &r}
}
