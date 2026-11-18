// Boundary sketch (llama capability vertical): owns closed LlamaModelObs
// (private, cloned), pure extractor from decoded row/props maps, and pure
// ObservedLlama into a private Contract. Inputs: maps (only here, with
// json.Number), mode, wall at publication. Outputs: closed obs + Contract
// with reported/inferred leaves + llama.models/props sources. No net/fs/
// clock/lock/worker/registry here. Single/legacy use usable props;
// router/swap ignore them. Present [] arch reports false; absent stays
// unknown. Exact-bool only; stream presence proves streaming.
package capability

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var llamaIntRe = regexp.MustCompile(`^-?[0-9]+$`)

// LlamaModelObs is one probed row's closed observation. All fields are
// private; discovery and router carry the same value without accessing
// them. Nil means no observed evidence (cache restore). Use Clone to copy.
type LlamaModelObs struct {
	mode           string
	propsAvailable bool
	propsBuild     *string
	archInPresent  bool
	archIn         []string
	archOutPresent bool
	archOut        []string
	vision         *bool
	audio          *bool
	video          *bool
	legacyMulti    bool
	toolsSup       *bool
	toolsPar       *bool
	effortSup      *bool
	streamPresent  bool
	samplingLoc    string
	sampling       map[string]Value
	rowMaxModelLen *int64
	metaNCtx       *int64
	metaNCtxTrain  *int64
	metaMaxIn      *int64
	metaMaxOut     *int64
	rowMaxIn       *int64
	rowMaxOut      *int64
	rowCtxLen      *int64
	metadataMaxIn  *int64
	metadataMaxOut *int64
	propsNCtx      *int64
	propsNCtxTrain *int64
	propsMaxIn     *int64
	propsMaxOut    *int64
	defaultsNCtx   *int64
	defaultsNPr    *int64
	defaultsMaxT   *int64
	paramsNPr      *int64
	paramsMaxT     *int64
	argCtx         *int64
	argPredict     *int64
	argFit         *int64
}

// Clone deep-copies the observation (nil stays nil).
func (o *LlamaModelObs) Clone() *LlamaModelObs {
	if o == nil {
		return nil
	}
	cp := *o
	if o.propsBuild != nil {
		v := *o.propsBuild
		cp.propsBuild = &v
	}
	if o.archIn != nil {
		cp.archIn = append([]string(nil), o.archIn...)
	}
	if o.archOut != nil {
		cp.archOut = append([]string(nil), o.archOut...)
	}
	if o.vision != nil {
		v := *o.vision
		cp.vision = &v
	}
	if o.audio != nil {
		v := *o.audio
		cp.audio = &v
	}
	if o.video != nil {
		v := *o.video
		cp.video = &v
	}
	if o.toolsSup != nil {
		v := *o.toolsSup
		cp.toolsSup = &v
	}
	if o.toolsPar != nil {
		v := *o.toolsPar
		cp.toolsPar = &v
	}
	if o.effortSup != nil {
		v := *o.effortSup
		cp.effortSup = &v
	}
	if o.sampling != nil {
		cp.sampling = make(map[string]Value, len(o.sampling))
		for k, v := range o.sampling {
			cp.sampling[k] = v.clone()
		}
	}
	cp.rowMaxModelLen = copyIntPtr(o.rowMaxModelLen)
	cp.metaNCtx = copyIntPtr(o.metaNCtx)
	cp.metaNCtxTrain = copyIntPtr(o.metaNCtxTrain)
	cp.metaMaxIn = copyIntPtr(o.metaMaxIn)
	cp.metaMaxOut = copyIntPtr(o.metaMaxOut)
	cp.rowMaxIn = copyIntPtr(o.rowMaxIn)
	cp.rowMaxOut = copyIntPtr(o.rowMaxOut)
	cp.rowCtxLen = copyIntPtr(o.rowCtxLen)
	cp.metadataMaxIn = copyIntPtr(o.metadataMaxIn)
	cp.metadataMaxOut = copyIntPtr(o.metadataMaxOut)
	cp.propsNCtx = copyIntPtr(o.propsNCtx)
	cp.propsNCtxTrain = copyIntPtr(o.propsNCtxTrain)
	cp.propsMaxIn = copyIntPtr(o.propsMaxIn)
	cp.propsMaxOut = copyIntPtr(o.propsMaxOut)
	cp.defaultsNCtx = copyIntPtr(o.defaultsNCtx)
	cp.defaultsNPr = copyIntPtr(o.defaultsNPr)
	cp.defaultsMaxT = copyIntPtr(o.defaultsMaxT)
	cp.paramsNPr = copyIntPtr(o.paramsNPr)
	cp.paramsMaxT = copyIntPtr(o.paramsMaxT)
	cp.argCtx = copyIntPtr(o.argCtx)
	cp.argPredict = copyIntPtr(o.argPredict)
	cp.argFit = copyIntPtr(o.argFit)
	return &cp
}

func copyIntPtr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// uniqueStrings validates Python _strings: a list of unique nonempty
// strings. The empty list is present (reports false everywhere).
func uniqueStrings(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok || s == "" || seen[s] {
			return nil, false
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, true
}

func exactBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// exactInt parses one JSON number without float round-trip. Bools are
// rejected. Integer literals use 64-bit parsing (out-of-range is unknown);
// fractional literals must be finite, integral and exactly representable.
func exactInt(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		return 0, false
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		const maxInt64Float = 9223372036854775808.0
		if n < -maxInt64Float || n >= maxInt64Float {
			return 0, false
		}
		got := int64(n)
		if float64(got) != n {
			return 0, false
		}
		return got, true
	case json.Number:
		s := strings.TrimSpace(n.String())
		if s == "" {
			return 0, false
		}
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
				return 0, false
			}
			const maxInt64Float = 9223372036854775808.0
			if f < -maxInt64Float || f >= maxInt64Float {
				return 0, false
			}
			got := int64(f)
			if float64(got) != f {
				return 0, false
			}
			return got, true
		}
		if !llamaIntRe.MatchString(s) {
			return 0, false
		}
		got, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return got, true
	default:
		return 0, false
	}
}

func limitPtr(v any) *int64 {
	n, ok := exactInt(v)
	if !ok {
		return nil
	}
	return &n
}

// llamaArg ports _llama_arg: last occurrence wins including equals syntax;
// a final invalid/auto value clears (never revives an earlier value).
func llamaArg(args any, flags []string) *int64 {
	list, ok := args.([]any)
	if !ok {
		return nil
	}
	set := map[string]bool{}
	for _, f := range flags {
		set[f] = true
	}
	var result *int64
	var has bool
	for i := 0; i < len(list); i++ {
		arg, ok := list[i].(string)
		if !ok {
			continue
		}
		flag, value, hasValue := strings.Cut(arg, "=")
		if !set[flag] {
			continue
		}
		var raw *string
		if !hasValue {
			if i+1 < len(list) {
				if s, ok := list[i+1].(string); ok {
					raw = &s
				} else {
					// Non-string next arg is an invalid value, not a skip.
					has = true
					result = nil
					continue
				}
			} else {
				has = true
				result = nil
				continue
			}
		} else {
			raw = &value
		}
		has = true
		if raw != nil && llamaIntRe.MatchString(*raw) {
			if n, err := strconv.ParseInt(*raw, 10, 64); err == nil {
				v := n
				result = &v
			} else {
				result = nil
			}
		} else {
			result = nil
		}
	}
	if !has {
		return nil
	}
	return result
}

// samplingNumber ports _number for sampling defaults: exact int64 or
// finite float64 (bool rejected, no coercion).
func samplingNumber(v any) (Value, bool) {
	switch n := v.(type) {
	case bool:
		return Value{}, false
	case int64:
		return IntValue(n), true
	case int:
		return IntValue(int64(n)), true
	case float64:
		return FloatValue(n)
	case json.Number:
		s := strings.TrimSpace(n.String())
		if s == "" {
			return Value{}, false
		}
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return Value{}, false
			}
			return FloatValue(f)
		}
		if !llamaIntRe.MatchString(s) {
			return Value{}, false
		}
		got, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return Value{}, false
		}
		return IntValue(got), true
	default:
		return Value{}, false
	}
}

// ExtractLlamaObs is the ONLY row/props map boundary after JSON decode.
// It accepts existing decoded maps (nil allowed), validates and closes
// them into a privately owned observation; no raw maps survive. Numbers
// use json.Number exactness; invalid values become unknown (nil), never
// zero/false. Router/swap modes ignore global props (no fan-out).
func ExtractLlamaObs(row map[string]any, props map[string]any, mode string) *LlamaModelObs {
	obs := &LlamaModelObs{mode: mode, sampling: map[string]Value{}}
	usable := mode == "single" || mode == "legacy"
	var pe map[string]any
	if usable && props != nil {
		pe = props
	}
	if usable && len(pe) > 0 {
		obs.propsAvailable = true
	} else {
		pe = nil
	}
	if pe != nil {
		if s, ok := pe["build_info"].(string); ok {
			v := s
			obs.propsBuild = &v
		}
	}
	var rowMap map[string]any
	if row != nil {
		rowMap = row
	}
	// Architecture lists.
	if rowMap != nil {
		if arch, ok := rowMap["architecture"].(map[string]any); ok {
			if v, present := uniqueStrings(arch["input_modalities"]); present {
				obs.archInPresent = true
				obs.archIn = v
			}
			if v, present := uniqueStrings(arch["output_modalities"]); present {
				obs.archOutPresent = true
				obs.archOut = v
			}
		}
		// Legacy exact "multimodal".
		if mode == "legacy" {
			switch caps := rowMap["capabilities"].(type) {
			case []any:
				for _, item := range caps {
					if s, ok := item.(string); ok && s == "multimodal" {
						obs.legacyMulti = true
						break
					}
				}
			case []string:
				for _, s := range caps {
					if s == "multimodal" {
						obs.legacyMulti = true
						break
					}
				}
			}
		}
	}
	// Props partial flags.
	if pe != nil {
		if mods, ok := pe["modalities"].(map[string]any); ok {
			if b, ok := exactBool(mods["vision"]); ok {
				obs.vision = copyBoolPtr(&b)
			}
			if b, ok := exactBool(mods["audio"]); ok {
				obs.audio = copyBoolPtr(&b)
			}
			if b, ok := exactBool(mods["video"]); ok {
				obs.video = copyBoolPtr(&b)
			}
		}
		if caps, ok := pe["chat_template_caps"].(map[string]any); ok {
			var tools []bool
			if b, ok := exactBool(caps["supports_tools"]); ok {
				tools = append(tools, b)
			}
			if b, ok := exactBool(caps["supports_tool_calls"]); ok {
				tools = append(tools, b)
			}
			if len(tools) > 0 {
				all := true
				for _, b := range tools {
					all = all && b
				}
				obs.toolsSup = copyBoolPtr(&all)
			}
			if b, ok := exactBool(caps["supports_parallel_tool_calls"]); ok {
				obs.toolsPar = copyBoolPtr(&b)
			}
			if b, ok := exactBool(caps["supports_reasoning_effort"]); ok {
				obs.effortSup = copyBoolPtr(&b)
			}
		}
	}
	// Defaults/params split.
	var defaultsMap, paramsMap map[string]any
	nested := false
	if pe != nil {
		if d, ok := pe["default_generation_settings"].(map[string]any); ok {
			defaultsMap = d
			if p, ok := d["params"].(map[string]any); ok {
				paramsMap = p
				nested = true
			} else {
				paramsMap = d
			}
		}
	}
	if nested {
		obs.samplingLoc = "default_generation_settings.params."
	} else {
		obs.samplingLoc = "default_generation_settings."
	}
	if paramsMap != nil {
		if b, ok := paramsMap["stream"].(bool); ok {
			_ = b
			obs.streamPresent = true
		}
		for _, key := range sampling {
			raw, has := paramsMap[key]
			if !has {
				continue
			}
			if key == "samplers" || key == "dry_sequence_breakers" {
				ss, ok := uniqueStrings(raw)
				if !ok {
					continue
				}
				v, ok := StringListValue(ss)
				if !ok {
					continue
				}
				obs.sampling[key] = v
				continue
			}
			if v, ok := samplingNumber(raw); ok {
				obs.sampling[key] = v
			}
		}
	}
	// Limit inputs.
	var metaMap, metadataMap map[string]any
	if rowMap != nil {
		if m, ok := rowMap["meta"].(map[string]any); ok {
			metaMap = m
		}
		if m, ok := rowMap["metadata"].(map[string]any); ok {
			metadataMap = m
		}
		obs.rowMaxModelLen = limitPtr(rowMap["max_model_len"])
		obs.rowMaxIn = limitPtr(rowMap["max_input_tokens"])
		obs.rowMaxOut = limitPtr(rowMap["max_output_tokens"])
		obs.rowCtxLen = limitPtr(rowMap["context_length"])
		var args any
		if st, ok := rowMap["status"].(map[string]any); ok {
			args = st["args"]
		}
		obs.argCtx = llamaArg(args, []string{"--ctx-size", "-c"})
		obs.argPredict = llamaArg(args, []string{"--predict", "--n-predict", "-n"})
		obs.argFit = llamaArg(args, []string{"--fit-ctx", "-fitc"})
	}
	if metaMap != nil {
		obs.metaNCtx = limitPtr(metaMap["n_ctx"])
		obs.metaNCtxTrain = limitPtr(metaMap["n_ctx_train"])
		obs.metaMaxIn = limitPtr(metaMap["max_input_tokens"])
		obs.metaMaxOut = limitPtr(metaMap["max_output_tokens"])
	}
	if metadataMap != nil {
		obs.metadataMaxIn = limitPtr(metadataMap["max_input_tokens"])
		obs.metadataMaxOut = limitPtr(metadataMap["max_output_tokens"])
	}
	if pe != nil {
		obs.propsNCtx = limitPtr(pe["n_ctx"])
		obs.propsNCtxTrain = limitPtr(pe["n_ctx_train"])
		obs.propsMaxIn = limitPtr(pe["max_input_tokens"])
		obs.propsMaxOut = limitPtr(pe["max_output_tokens"])
	}
	if defaultsMap != nil {
		obs.defaultsNCtx = limitPtr(defaultsMap["n_ctx"])
		obs.defaultsNPr = limitPtr(defaultsMap["n_predict"])
		obs.defaultsMaxT = limitPtr(defaultsMap["max_tokens"])
	}
	if nested && paramsMap != nil {
		obs.paramsNPr = limitPtr(paramsMap["n_predict"])
		obs.paramsMaxT = limitPtr(paramsMap["max_tokens"])
	}
	return obs
}
