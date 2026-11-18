package capability

import "strings"

// ObservedLlama builds the closed contract from one observation at one
// wall time. It starts from Empty("openai-chat"), applies only the
// observed reported/inferred rules, sets llama.models/props sources, and
// ports the observed _llama_limits chain without fallback or overrides.
// Router/swap modes carry no props-derived leaves. Effort true infers
// reasoning support plus the native openai-chat/reasoning_effort shape;
// no values, toggle or budget follow.
func ObservedLlama(obs *LlamaModelObs, wall float64) Contract {
	base := Empty("openai-chat")
	base.limitConstraints = map[string]Fact{}
	if obs == nil {
		return base
	}
	w := wall
	modelsVer := "upstream report"
	propsVer := "upstream report"
	endpoint := "/props?autoload=false"
	avail := obs.propsAvailable
	base.sources["llama.models"] = &Source{retrievedAt: &w, verification: &modelsVer}
	var retrieved *float64
	if obs.propsAvailable {
		retrieved = &w
	}
	var build *string
	if obs.propsBuild != nil {
		v := *obs.propsBuild
		build = &v
	}
	base.sources["llama.props"] = &Source{
		retrievedAt: retrieved, endpoint: &endpoint,
		available: &avail, verification: &propsVer, serverBuild: build,
	}
	putFact := func(path string, v Value, source, kind string) {
		s := source
		base.put(path, v, &s, kind)
	}
	// Architecture: present (incl []) reports false per missing modality.
	if obs.archInPresent {
		set := map[string]bool{}
		for _, m := range obs.archIn {
			set[m] = true
		}
		for _, m := range modalities {
			src := "llama.models.architecture.input_modalities"
			putFact("modalities.input."+m, BoolValue(set[m]), src, KindReported)
		}
	}
	if obs.archOutPresent {
		set := map[string]bool{}
		for _, m := range obs.archOut {
			set[m] = true
		}
		for _, m := range modalities {
			src := "llama.models.architecture.output_modalities"
			putFact("modalities.output."+m, BoolValue(set[m]), src, KindReported)
		}
	}
	if obs.vision != nil {
		putFact("modalities.input.image", BoolValue(*obs.vision), "llama.props.modalities.vision", KindReported)
	}
	if obs.audio != nil {
		putFact("modalities.input.audio", BoolValue(*obs.audio), "llama.props.modalities.audio", KindReported)
	}
	if obs.video != nil {
		putFact("modalities.input.video", BoolValue(*obs.video), "llama.props.modalities.video", KindReported)
	}
	if obs.mode == "legacy" && obs.legacyMulti {
		putFact("modalities.input.image", BoolValue(true), "llama.models.capabilities", KindInferred)
	}
	if obs.toolsSup != nil {
		putFact("tools.supported", BoolValue(*obs.toolsSup), "llama.props.chat_template_caps.supports_tools/supports_tool_calls", KindReported)
	}
	if obs.toolsPar != nil {
		putFact("tools.parallel", BoolValue(*obs.toolsPar), "llama.props.chat_template_caps.supports_parallel_tool_calls", KindReported)
	}
	if obs.effortSup != nil {
		putFact("reasoning.effort.supported", BoolValue(*obs.effortSup), "llama.props.chat_template_caps.supports_reasoning_effort", KindReported)
		if *obs.effortSup {
			putFact("reasoning.supported", BoolValue(true), "llama.props.chat_template_caps.supports_reasoning_effort", KindReported)
			if np, ok := NewNativeParam("openai-chat", "reasoning_effort", nil); ok {
				putFact("reasoning.effort.native_parameter", NativeValue(np), "llama.props.chat_template_caps.supports_reasoning_effort", KindInferred)
			}
		}
	}
	for _, key := range sampling {
		v, ok := obs.sampling[key]
		if !ok {
			continue
		}
		src := "llama.props." + obs.samplingLoc + key
		putFact("sampling."+key+".supported", BoolValue(true), src, KindInferred)
		putFact("sampling."+key+".default", v.clone(), src, KindReported)
		if np, ok := NewNativeParam("openai-chat", key, nil); ok {
			putFact("sampling."+key+".native_parameter", NativeValue(np), src, KindInferred)
		}
	}
	if obs.streamPresent {
		src := "llama.props." + obs.samplingLoc + "stream"
		putFact("streaming", BoolValue(true), src, KindInferred)
	}
	// Limits: observed chain only (no fallback, no overrides).
	type limCand struct {
		v *int64
		s string
	}
	firstValid := func(min int64, cands ...limCand) (*int64, string, bool) {
		for _, c := range cands {
			if c.v == nil || *c.v < min {
				continue
			}
			return c.v, c.s, true
		}
		return nil, "", false
	}
	setRep := func(k string, v int64, src string) {
		base.limits[k] = Fact{value: IntValue(v), source: strPtr(src), kind: KindReported}
	}
	if obs.mode == "legacy" {
		if v, src, ok := firstValid(1, limCand{obs.rowMaxModelLen, "/v1/models.max_model_len"}, limCand{obs.metaNCtx, "/v1/models.meta.n_ctx"}, limCand{obs.defaultsNCtx, "/props.default_generation_settings.n_ctx"}, limCand{obs.propsNCtx, "/props.n_ctx"}); ok {
			setRep("context_window", *v, src)
		} else if obs.argCtx != nil && *obs.argCtx > 0 {
			base.limits["context_window"] = Fact{value: IntValue(*obs.argCtx), source: strPtr("/v1/models.status.args.ctx-size"), kind: KindInferred}
		}
	} else if v, src, ok := firstValid(1, limCand{obs.metaNCtx, "/v1/models.meta.n_ctx"}, limCand{obs.defaultsNCtx, "/props.default_generation_settings.n_ctx"}, limCand{obs.propsNCtx, "/props.n_ctx"}); ok {
		setRep("context_window", *v, src)
	} else if obs.argCtx != nil && *obs.argCtx > 0 {
		base.limits["context_window"] = Fact{value: IntValue(*obs.argCtx), source: strPtr("/v1/models.status.args.ctx-size"), kind: KindInferred}
	}
	if obs.mode == "router" {
		if v, src, ok := firstValid(1, limCand{obs.metaNCtxTrain, "/v1/models.meta.n_ctx_train"}, limCand{obs.rowCtxLen, "/v1/models.context_length"}, limCand{obs.propsNCtxTrain, "/props.n_ctx_train"}); ok {
			setRep("trained_context_window", *v, src)
		}
	} else if v, src, ok := firstValid(1, limCand{obs.metaNCtxTrain, "/v1/models.meta.n_ctx_train"}, limCand{obs.propsNCtxTrain, "/props.n_ctx_train"}); ok {
		setRep("trained_context_window", *v, src)
	}
	if v, src, ok := firstValid(1, limCand{obs.metaMaxIn, "/v1/models.meta.max_input_tokens"}, limCand{obs.rowMaxIn, "/v1/models.max_input_tokens"}, limCand{obs.metadataMaxIn, "/v1/models.metadata.max_input_tokens"}, limCand{obs.propsMaxIn, "/props.max_input_tokens"}); ok {
		setRep("max_input_tokens", *v, src)
	}
	if v, src, ok := firstValid(1, limCand{obs.metaMaxOut, "/v1/models.meta.max_output_tokens"}, limCand{obs.rowMaxOut, "/v1/models.max_output_tokens"}, limCand{obs.metadataMaxOut, "/v1/models.metadata.max_output_tokens"}, limCand{obs.propsMaxOut, "/props.max_output_tokens"}); ok {
		setRep("max_output_tokens", *v, src)
	}
	for _, c := range []limCand{
		{obs.paramsNPr, "/props.default_generation_settings.params.n_predict"},
		{obs.defaultsNPr, "/props.default_generation_settings.n_predict"},
		{obs.paramsMaxT, "/props.default_generation_settings.params.max_tokens"},
		{obs.defaultsMaxT, "/props.default_generation_settings.max_tokens"},
		{obs.argPredict, "/v1/models.status.args.n-predict"},
	} {
		if c.v == nil || *c.v < -2 {
			continue
		}
		kind := KindReported
		if strings.HasPrefix(c.s, "/v1/models.status.args") {
			kind = KindInferred
		}
		if *c.v < 0 {
			special := LimitSpecialUntilContext
			if *c.v == -1 {
				special = LimitSpecialUnlimited
			}
			base.limits["default_output_tokens"] = Fact{value: Null(), source: strPtr(c.s), kind: kind, special: &special}
		} else {
			base.limits["default_output_tokens"] = Fact{value: IntValue(*c.v), source: strPtr(c.s), kind: kind}
		}
		break
	}
	if obs.argFit != nil && *obs.argFit > 0 {
		base.limitConstraints["fit_context_min"] = Fact{
			value: IntValue(*obs.argFit), source: strPtr("/v1/models.status.args.fit-ctx"), kind: KindConstraint,
		}
	}
	return base
}
