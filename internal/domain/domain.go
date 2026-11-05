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
	LibraryFolderID    int               `json:"libraryFolderId"`
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
	Match          int            `json:"match"`
	Reason         string         `json:"reason"`
	FieldSources   map[string]int `json:"fieldSources"`
	WorkSources    []int          `json:"workSources,omitempty"`
	EditionSources []int          `json:"editionSources,omitempty"`
	CoverSource    int            `json:"coverSource"`
	Model          string         `json:"model,omitempty"`
	InputTokens    int            `json:"inputTokens,omitempty"`
	OutputTokens   int            `json:"outputTokens,omitempty"`
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
	SeriesReason  string            `json:"seriesReason,omitempty"`
	Visual        *VisualReport     `json:"visual,omitempty"`
}

// Evidence is one volume's source material within a series plan.
type Evidence struct {
	Book       Book        `json:"book"`
	Candidates []Candidate `json:"candidates"`
	Warnings   []string    `json:"warnings,omitempty"`
}

type CoverObservation struct {
	BookID      int      `json:"bookId"`
	Source      int      `json:"source"`
	Family      string   `json:"family"`
	VisibleText string   `json:"visibleText"`
	Issues      []string `json:"issues"`
}

type VisualReport struct {
	Model        string             `json:"model"`
	Summary      string             `json:"summary"`
	Covers       []CoverObservation `json:"covers"`
	InputTokens  int                `json:"inputTokens"`
	OutputTokens int                `json:"outputTokens"`
}

// Discovery groups records for editorial attention, without asserting that their
// current metadata is correct or that every member belongs to one series.
type Discovery struct {
	Warnings     []string `json:"warnings,omitempty"`
	Groups       []Group  `json:"groups"`
	Model        string   `json:"model,omitempty"`
	InputTokens  int      `json:"inputTokens,omitempty"`
	OutputTokens int      `json:"outputTokens,omitempty"`
}

type Group struct {
	BookIDs []int  `json:"bookIds"`
	Reason  string `json:"reason"`
}
