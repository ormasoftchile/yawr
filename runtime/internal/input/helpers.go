package input

import "github.com/ormasoftchile/yawr/runtime/pkg/input"

func defaultCacheKey(req input.InputRequest) string {
	if req.StepID == "" {
		return req.VarName
	}
	if req.VarName == "" {
		return req.StepID
	}
	return req.StepID + ":" + req.VarName
}

func ensureResponse(req input.InputRequest, resp *input.InputResponse, source string) *input.InputResponse {
	if resp == nil {
		return nil
	}
	if resp.Source == "" {
		resp.Source = source
	}
	if resp.CacheKey == "" {
		resp.CacheKey = defaultCacheKey(req)
	}
	if req.Sensitive {
		resp.Sensitive = true
	}
	return resp
}
