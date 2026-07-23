package api

import "fmt"

// reportAllTimeStart is the earliest date the Reports API accepts (its allowed
// range starts at 2006-01-01), used as the default "all time" lower bound when
// no date filtering is requested.
const reportAllTimeStart = "2006-01-01"

// SummaryTask is a single task's total tracked time as reported by the Reports
// API summary endpoint. Seconds is the total tracked duration across the whole
// requested date range; TaskID/Name come from the summary sub-group.
type SummaryTask struct {
	TaskID  int64
	Name    string
	Seconds int64
}

// summaryRequest is the request body for the summary/time_entries endpoint. Only
// the fields tg needs are sent; the endpoint accepts many more optional filters.
type summaryRequest struct {
	StartDate   string `json:"start_date"`
	EndDate     string `json:"end_date"`
	Grouping    string `json:"grouping"`
	SubGrouping string `json:"sub_grouping"`
}

// summaryResponse mirrors the summary/time_entries response: a list of groups,
// each holding sub-groups. With grouping=projects and sub_grouping=tasks, each
// group is a project and each sub-group is a task (id + title) with its total
// tracked seconds.
type summaryResponse struct {
	Groups []struct {
		ID        *int64 `json:"id"`
		SubGroups []struct {
			ID      *int64 `json:"id"`
			Title   string `json:"title"`
			Seconds int64  `json:"seconds"`
		} `json:"sub_groups"`
	} `json:"groups"`
}

// SummaryByTask returns per-task tracked totals for the workspace over
// [startDate, endDate] (inclusive, "YYYY-MM-DD"). An empty startDate defaults to
// the earliest date the Reports API allows, giving an all-time total. Totals
// come straight from the Reports API (POST
// /workspace/{workspace_id}/summary/time_entries, grouping=projects,
// sub_grouping=tasks); nothing is read from or written to the local store.
// Sub-groups without a task (e.g. untitled/no-task time) are skipped, and
// sub-groups sharing a task id are summed.
func (c *Client) SummaryByTask(workspaceID int64, startDate, endDate string) ([]SummaryTask, error) {
	if startDate == "" {
		startDate = reportAllTimeStart
	}
	req := summaryRequest{
		StartDate:   startDate,
		EndDate:     endDate,
		Grouping:    "projects",
		SubGrouping: "tasks",
	}
	var resp summaryResponse
	path := fmt.Sprintf("/workspace/%d/summary/time_entries", workspaceID)
	if err := c.doReports("POST", path, req, &resp); err != nil {
		return nil, err
	}

	// Flatten sub-groups into per-task totals, summing any duplicate task ids
	// that appear under more than one group and preserving first-seen order.
	var out []SummaryTask
	index := map[int64]int{}
	for _, g := range resp.Groups {
		for _, sg := range g.SubGroups {
			if sg.ID == nil || sg.Title == "" {
				continue // no task -> nothing to match a fragment against
			}
			id := *sg.ID
			if i, ok := index[id]; ok {
				out[i].Seconds += sg.Seconds
				continue
			}
			index[id] = len(out)
			out = append(out, SummaryTask{TaskID: id, Name: sg.Title, Seconds: sg.Seconds})
		}
	}
	return out, nil
}
