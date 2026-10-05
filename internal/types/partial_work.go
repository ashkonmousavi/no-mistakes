package types

// PartialWork describes local unfinished work, never a publishable run head.
type PartialWork struct {
	Version              int    `json:"version"`
	StopID               string `json:"stop_id"`
	RunID                string `json:"source_run"`
	RepoID               string `json:"repo_id"`
	Branch               string `json:"branch"`
	Step                 string `json:"step"`
	Selection            string `json:"selection,omitempty"`
	ParentHead           string `json:"parent_head"`
	Ref                  string `json:"ref,omitempty"`
	SHA                  string `json:"sha,omitempty"`
	IndexSHA             string `json:"index_sha,omitempty"`
	State                string `json:"state"`
	Path                 string `json:"path,omitempty"`
	Reason               string `json:"reason,omitempty"`
	ConsumedBy           string `json:"consumed_by,omitempty"`
	ConsumptionCompleted bool   `json:"consumption_completed,omitempty"`
}

// FixProgress reports applied repairs; independent validation owns completion.
type FixProgress struct {
	Applied           int    `json:"applied"`
	Total             int    `json:"total"`
	Current           string `json:"current,omitempty"`
	SavedHead         string `json:"saved_head,omitempty"`
	Step              string `json:"step"`
	Selection         string `json:"selection"`
	ValidationPending bool   `json:"validation_pending"`
}
