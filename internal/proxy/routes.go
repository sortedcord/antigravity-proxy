package proxy

import "strings"

type routeKind uint8

const (
	routeUnknown routeKind = iota
	routeHealth
	routeRawModels
	routeLimit
	routeUsage
	routeAccount
	routeNativeList
	routeNativeModel
)

type apiRoute struct {
	kind      routeKind
	label     string
	model     string
	action    string
	hasAction bool
}

// resolveRoute is shared by dispatch and access logging. Methods are checked by
// handlers so their existing status codes and native Gemini error shapes remain
// unchanged; ServeMux's implicit redirects/405 responses would change those APIs.
func resolveRoute(path string) apiRoute {
	for _, route := range []struct {
		path string
		kind routeKind
	}{
		{path: "/health", kind: routeHealth},
		{path: "/models", kind: routeRawModels},
		{path: "/status/limit", kind: routeLimit},
		{path: "/status/usage", kind: routeUsage},
		{path: "/status/account", kind: routeAccount},
		{path: "/v1beta/models", kind: routeNativeList},
	} {
		if path == route.path {
			return apiRoute{kind: route.kind, label: path}
		}
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		model, action, hasAction := strings.Cut(strings.TrimPrefix(path, "/v1beta/models/"), ":")
		return apiRoute{kind: routeNativeModel, label: "/v1beta/models/{model}", model: model, action: action, hasAction: hasAction}
	}
	return apiRoute{label: "unmatched"}
}

func (route apiRoute) accessLabel() (string, string) {
	if route.kind != routeNativeModel {
		return route.label, ""
	}
	// Do not log arbitrary caller-controlled path segments or secrets.
	if route.model == "" || len(route.model) > 128 {
		return "unmatched", ""
	}
	for _, c := range route.model {
		isModelChar := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !isModelChar {
			return "unmatched", ""
		}
	}
	switch route.action {
	case "":
		return route.label, route.model
	case "generateContent", "streamGenerateContent":
		return route.label + ":" + route.action, route.model
	default:
		return "unmatched", ""
	}
}
