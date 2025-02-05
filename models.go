package princepdf

import "fmt"

// NewJob instantiates a new job to send to prince using the provided source
// HTML data file.
func NewJob() *Job {
	j := new(Job)
	j.Files = make(map[string][]byte)
	return j
}

// Job defines the structure of a request to be sent to the prince controller.
type Job struct {
	Input    *Input            `json:"input" form:"input"`
	PDF      *PDF              `json:"pdf,omitempty" form:"pdf"`
	Metadata *Metadata         `json:"metadata,omitempty" form:"metadata"`
	Files    map[string][]byte `json:"files,omitempty"`

	// reply is used to send the output back to the caller
	reply chan *output
}

func (j *Job) request() *jobRequest {
	req := &jobRequest{
		Input:            new(Input),
		PDF:              j.PDF,
		Metadata:         j.Metadata,
		JobResourceCount: 0,
		resources:        make([][]byte, len(j.Files)),
	}
	*req.Input = *j.Input // copy

	i := 0
	idxMap := make(map[string]int)
	for n, r := range j.Files {
		idxMap[n] = i
		req.resources[i] = r
		i++
	}
	req.JobResourceCount = i

	// update the input to point to the correct resources
	if i, ok := idxMap[req.Input.Src]; ok {
		req.Input.Src = fmt.Sprintf(strJobResource, i)
	}
	for x, fn := range req.Input.Styles {
		if i, ok := idxMap[fn]; ok {
			req.Input.Styles[x] = fmt.Sprintf(strJobResource, i)
		}
	}
	for x, fn := range req.Input.Scripts {
		if i, ok := idxMap[fn]; ok {
			req.Input.Scripts[x] = fmt.Sprintf(strJobResource, i)
		}
	}

	if req.PDF != nil {
		for x, a := range req.PDF.Attach {
			if i, ok := idxMap[a.Filename]; ok {
				a.URL = fmt.Sprintf(strJobResource, i)
			}
			req.PDF.Attach[x] = a
		}
	}

	return req
}

// jobRequest represents the data structure expected by prince.
type jobRequest struct {
	Input            *Input    `json:"input"`
	PDF              *PDF      `json:"pdf,omitempty"`
	Metadata         *Metadata `json:"metadata,omitempty"`
	JobResourceCount int       `json:"job-resource-count"`
	resources        [][]byte
}

// Input defines the structured input to be sent to the prince controller.
type Input struct {
	Src                 string   `json:"src,omitempty"`
	Type                string   `json:"type,omitempty"`
	Base                string   `json:"base,omitempty"`
	Media               string   `json:"media,omitempty"`
	Styles              []string `json:"styles,omitempty"`
	Scripts             []string `json:"scripts,omitempty"`
	DefaultStyle        bool     `json:"default-style,omitempty"`
	AuthorStyle         bool     `json:"author-style,omitempty"`
	Javascript          bool     `json:"javascript,omitempty"`
	MaxPasses           int      `json:"max-passes,omitempty"`
	Iframes             bool     `json:"iframes,omitempty"`
	XInclude            bool     `json:"xinclude,omitempty"`
	XMLExternalEntities bool     `json:"xml-external-entities,omitempty"`
}

// PDF configures options to use when building the PDF. These are copied from the
// Prince documentation site.
type PDF struct {
	ColorOptions          string        `json:"color-options,omitempty"`
	EmbedFonts            bool          `json:"embed-fonts,omitempty"`
	SubsetFonts           bool          `json:"subset-fonts,omitempty"`
	ArtificialFonts       bool          `json:"artificial-fonts,omitempty"`
	ForceIdentityEncoding bool          `json:"force-identity-encoding,omitempty"`
	Compress              bool          `json:"compress,omitempty"`
	ObjectStreams         bool          `json:"object-streams,omitempty"`
	Encrypt               *Encrypt      `json:"encrypt,omitempty"`
	PDFProfile            string        `json:"pdf-profile,omitempty"`
	PDFOutputIntent       string        `json:"pdf-output-intent,omitempty"`
	FallbackCMYKProfile   string        `json:"fallback-cmyk-profile,omitempty"`
	ColorConversion       string        `json:"color-conversion,omitempty"`
	PDFScript             string        `json:"pdf-script,omitempty"`
	PDFID                 string        `json:"pdf-id,omitempty"`
	PDFLang               string        `json:"pdf-lang,omitempty"`
	PDFXMP                string        `json:"pdf-xmp,omitempty"`
	PDFXMLMetadata        bool          `json:"pdf-xml-metadata,omitempty"`
	TaggedPDF             string        `json:"tagged-pdf,omitempty"`
	Attach                []*Attachment `json:"attach,omitempty"`
}

// Encrypt defines options used to encrypt output PDF
type Encrypt struct {
	KeyBits                   int    `json:"key-bits,omitempty"`
	UserPassword              string `json:"user-password,omitempty"`
	OwnerPassword             string `json:"owner-password,omitempty"`
	DisallowPrint             bool   `json:"disallow-print,omitempty"`
	DisallowModify            bool   `json:"disallow-modify,omitempty"`
	DisallowCopy              bool   `json:"disallow-copy,omitempty"`
	DisallowAnnotate          bool   `json:"disallow-annotate,omitempty"`
	AllowCopyForAccessibility bool   `json:"allow-copy-for-accessibility,omitempty"`
	AllowAssembly             bool   `json:"allow-assembly,omitempty"`
}

// Attachment indicates and embedded file inside the PDF
type Attachment struct {
	URL          string `json:"url,omitempty"`
	Filename     string `json:"filename,omitempty"`
	Description  string `json:"description,omitempty"`
	Relationship string `json:"relationship,omitempty"`
}

// Metadata for additional data to include in the PDF
type Metadata struct {
	Title    string `json:"title,omitempty"`
	Subject  string `json:"subject,omitempty"`
	Author   string `json:"author,omitempty"`
	Keywords string `json:"keywords,omitempty"`
	Creator  string `json:"creator,omitempty"`
}
