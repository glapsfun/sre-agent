package sources

// DecodeSelection accepts the same nonsecret descriptors used for known sources.
// It does not grant approval or resolve credentials.
func DecodeSelection(raw []byte) ([]Source, error) {
	var value struct {
		Sources []Source `json:"sources"`
	}
	if strictScoutJSON(raw, &value) != nil || value.Sources == nil || len(value.Sources) > 64 {
		return nil, errInvalidSource
	}
	seen := map[string]bool{}
	for _, source := range value.Sources {
		if ValidateSource(source) != nil || seen[source.ID] {
			return nil, errInvalidSource
		}
		seen[source.ID] = true
	}
	return value.Sources, nil
}
