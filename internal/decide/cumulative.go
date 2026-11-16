package decide

// Cumulative is what one login role has touched in a table within the window,
// against the limit. It is stored as stet.decisions.cumulative.
type Cumulative struct {
	Used    *int64 `json:"used,omitempty"`    // rows in the window, without this write
	Planned *int64 `json:"planned,omitempty"` // rows this write is expected to touch
	Limit   int64  `json:"limit,omitempty"`
	WindowS int    `json:"window_s,omitempty"`
	Error   string `json:"error,omitempty"` // set when the usage is not known; the write is then held (impact:cumulative_unknown)
}

// Over reports whether this write takes the role past the limit. An unknown
// usage is never over; see Unknown.
func (c *Cumulative) Over() bool {
	if c == nil || c.Error != "" || c.Used == nil || c.Planned == nil {
		return false
	}
	return *c.Used+*c.Planned > c.Limit
}

// Unknown reports that the usage could not be read (Error is set).
func (c *Cumulative) Unknown() bool {
	return c != nil && c.Error != ""
}
