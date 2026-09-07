package api

import (
	"sort"

	"github.com/presmihaylov/openchatter/models"
)

// Reciprocal rank fusion with the text leg weighted 2x. With k = 60 and 50
// rows per leg, the worst text hit (2/110) still beats the best semantic-only
// hit (1/61), so exact matches always lead and semantic ones fill in behind.
const (
	fuseK        = 60.0
	fuseTextW    = 2.0
	fuseLegLimit = 50
	// a nearest-neighbour leg always returns something; below this cosine
	// similarity a row is noise, not a related message (unrelated pairs on
	// text-embedding-3-small sit around 0.1-0.25, related ones above 0.4)
	semanticFloor = 0.3
)

func aboveFloor(rows []models.SearchResult) []models.SearchResult {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Score >= semanticFloor {
			out = append(out, r)
		}
	}
	return out
}

func fuseResults(text, semantic []models.SearchResult) []models.SearchResult {
	type entry struct {
		res   models.SearchResult
		score float64
	}
	byID := map[string]*entry{}
	out := []*entry{}
	add := func(r models.SearchResult, rank int, weight float64, via string) {
		e, ok := byID[r.ID]
		if !ok {
			r.Via = via
			e = &entry{res: r}
			byID[r.ID] = e
			out = append(out, e)
		}
		if via == "text" {
			e.res.Via = via
		}
		e.score += weight / (fuseK + float64(rank+1))
	}
	for i, r := range text {
		add(r, i, fuseTextW, "text")
	}
	for i, r := range semantic {
		add(r, i, 1, "semantic")
	}
	// ties keep leg order, so the sort is stable and deterministic
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]models.SearchResult, len(out))
	for i, e := range out {
		e.res.Score = e.score
		res[i] = e.res
	}
	return res
}
