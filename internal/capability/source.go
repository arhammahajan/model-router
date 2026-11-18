package capability

// Source is typed provenance for one layer. RetrievedAt is an explicit
// optional wall time (Unix seconds); the first slice never sets it, so
// unobserved layers derive null age/stale. AgeSeconds/Stale derive at
// render and are never stored by publishers. The five review-provenance
// strings below are set only on the "go.review" source by CatalogReview:
// model_source plus the bundled review provenance (source/reviewed_on/
// repository_revision/snapshot_sha256). They are optional typed strings,
// cloned and copied like every other field; the wire names are
// model_source, source, reviewed_on, repository_revision, snapshot_sha256.
// serverBuild is the exact /props build_info string for llama.props only
// (nil means unknown); the wire name is server_build.
type Source struct {
	retrievedAt       *float64
	staleAfterSeconds *float64
	verification      *string
	endpoint          *string
	available         *bool
	applicable        *bool
	ageSeconds        *float64
	stale             *bool
	modelSource       *string
	reviewSource      *string
	reviewedOn        *string
	repoRevision      *string
	snapshotSHA       *string
	serverBuild       *string
}

// ModelSource returns a copy of the bundled row source (nil when absent).
func (s *Source) ModelSource() *string {
	if s == nil || s.modelSource == nil {
		return nil
	}
	v := *s.modelSource
	return &v
}

// ReviewProvenance returns copies of the bundled review provenance
// (source, reviewed_on, repository_revision, snapshot_sha256).
func (s *Source) ReviewProvenance() (source, reviewedOn, repo, sha *string) {
	if s == nil {
		return nil, nil, nil, nil
	}
	if s.reviewSource != nil {
		v := *s.reviewSource
		source = &v
	}
	if s.reviewedOn != nil {
		v := *s.reviewedOn
		reviewedOn = &v
	}
	if s.repoRevision != nil {
		v := *s.repoRevision
		repo = &v
	}
	if s.snapshotSHA != nil {
		v := *s.snapshotSHA
		sha = &v
	}
	return source, reviewedOn, repo, sha
}

func (s *Source) clone() *Source {
	if s == nil {
		return nil
	}
	out := *s
	if s.retrievedAt != nil {
		v := *s.retrievedAt
		out.retrievedAt = &v
	}
	if s.staleAfterSeconds != nil {
		v := *s.staleAfterSeconds
		out.staleAfterSeconds = &v
	}
	if s.verification != nil {
		v := *s.verification
		out.verification = &v
	}
	if s.endpoint != nil {
		v := *s.endpoint
		out.endpoint = &v
	}
	if s.available != nil {
		v := *s.available
		out.available = &v
	}
	if s.applicable != nil {
		v := *s.applicable
		out.applicable = &v
	}
	if s.ageSeconds != nil {
		v := *s.ageSeconds
		out.ageSeconds = &v
	}
	if s.stale != nil {
		v := *s.stale
		out.stale = &v
	}
	if s.modelSource != nil {
		v := *s.modelSource
		out.modelSource = &v
	}
	if s.reviewSource != nil {
		v := *s.reviewSource
		out.reviewSource = &v
	}
	if s.reviewedOn != nil {
		v := *s.reviewedOn
		out.reviewedOn = &v
	}
	if s.repoRevision != nil {
		v := *s.repoRevision
		out.repoRevision = &v
	}
	if s.snapshotSHA != nil {
		v := *s.snapshotSHA
		out.snapshotSHA = &v
	}
	if s.serverBuild != nil {
		v := *s.serverBuild
		out.serverBuild = &v
	}
	return &out
}
