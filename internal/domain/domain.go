package domain

import "encoding/json"

// Metadata is the bibliographic subset the librarian curates. Zero values mean unknown.
type Metadata struct {
	Title         string   `json:"title,omitempty"`
	Subtitle      string   `json:"subtitle,omitempty"`
	Description   string   `json:"description,omitempty"`
	Authors       []string `json:"authors,omitempty"`
	Publisher     string   `json:"publisher,omitempty"`
	PublishedDate string   `json:"publishedDate,omitempty"`
	PublishedYear int      `json:"publishedYear,omitempty"`
	Language      string   `json:"language,omitempty"`
	PageCount     int      `json:"pageCount,omitempty"`
	ISBN10        string   `json:"isbn10,omitempty"`
	ISBN13        string   `json:"isbn13,omitempty"`
	SeriesName    string   `json:"seriesName,omitempty"`
	SeriesIndex   string   `json:"seriesIndex,omitempty"`
	Genres        []string `json:"genres,omitempty"`
}

type File struct {
	ID           int    `json:"id"`
	Format       string `json:"format"`
	Role         string `json:"role"`
	AbsolutePath string `json:"absolutePath"`
	FileHash     string `json:"fileHash"`
	SizeBytes    int64  `json:"sizeBytes"`
}

type Book struct {
	ID                 int               `json:"id"`
	LibraryID          int               `json:"libraryId"`
	UpdatedAt          string            `json:"updatedAt"`
	Metadata           Metadata          `json:"metadata"`
	Files              []File            `json:"files"`
	ProviderIDs        map[string]string `json:"providerIds"`
	HardcoverEditionID string            `json:"hardcoverEditionId"`
	LockedFields       []string          `json:"lockedFields"`
}

type Candidate struct {
	Metadata
	Provider           string `json:"provider"`
	ProviderID         string `json:"providerId"`
	HardcoverEditionID string `json:"hardcoverEditionId,omitempty"`
	CoverURL           string `json:"coverUrl,omitempty"`
	CoverWidth         int    `json:"coverWidth,omitempty"`
	CoverHeight        int    `json:"coverHeight,omitempty"`
	SourceURL          string `json:"sourceUrl,omitempty"`
}

// Decision references candidates by one-based index. Zero preserves an existing field.
type Decision struct {
	Match        int            `json:"match"`
	SameWork     bool           `json:"sameWork"`
	SameEdition  bool           `json:"sameEdition"`
	NeedsReview  bool           `json:"needsReview"`
	Reason       string         `json:"reason"`
	FieldSources map[string]int `json:"fieldSources"`
	Model        string         `json:"model,omitempty"`
	InputTokens  int            `json:"inputTokens,omitempty"`
	OutputTokens int            `json:"outputTokens,omitempty"`
}

type Patch map[string]json.RawMessage

type Proposal struct {
	Book          Book              `json:"book"`
	Candidates    []Candidate       `json:"candidates"`
	Decision      Decision          `json:"decision"`
	Patch         Patch             `json:"patch"`
	CoverURL      string            `json:"coverUrl,omitempty"`
	Provenance    map[string]string `json:"provenance"`
	PolicyVersion string            `json:"policyVersion"`
	Warnings      []string          `json:"warnings,omitempty"`
}
