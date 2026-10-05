package types

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
