package wikibrief

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/ebonetti/wikiassignment"
	"github.com/ebonetti/wikidump"
	"github.com/remeh/sizedwaitgroup"

	"github.com/negapedia/wikibots"
	"github.com/negapedia/wikipage"

	errorsOnSteroids "github.com/pkg/errors"
)

//New digest the latest wikipedia dump of the specified language into the output channel.
//The revision channel of each page must be exhausted, doing otherwise may result in a deadlock.
//The ctx and fail together should behave in the same manner as if created with WithFail - https://godoc.org/github.com/ebonetti/ctxutils#WithFail
func New(ctx context.Context, fail func(err error) error, tmpDir, lang string) <-chan EvolvingPage {
	//Default value to a closed channel
	dummyPagesChan := make(chan EvolvingPage)
	close(dummyPagesChan)

	ID2Bot, err := wikibots.New(ctx, lang)
	if err != nil {
		fail(err)
		return dummyPagesChan
	}

	latestDump, err := wikidump.Latest(tmpDir, lang, "metahistory7zdump",
		"pagetable", "redirecttable", "categorylinkstable", "pagelinkstable")
	if err != nil {
		fail(err)
		return dummyPagesChan
	}
	it := latestDump.Open("metahistory7zdump")

	article2TopicID, err := getArticle2TopicID(ctx, tmpDir, lang)
	if err != nil {
		fail(err)
		return dummyPagesChan
	}

	simplePages := make(chan simpleEvolvingPage, pageBufferSize)
	go func() {
		defer close(simplePages)

		//limit the number of workers to prevent system from killing 7zip instances
		wg := sizedwaitgroup.New(pageBufferSize)

		var r io.ReadCloser
		for r, err = it(ctx); err == nil; r, err = it(ctx) {
			if err = wg.AddWithContext(ctx); err != nil {
				return //AddWithContext only fail if ctx is Done
			}

			go func(r io.ReadCloser) {
				defer wg.Done()
				defer r.Close()
				err := run(ctx, bBase{xml.NewDecoder(r), article2TopicID, ID2Bot, simplePages, &errorContext{0, filename(r)}})
				if err != nil {
					fail(err)
				}
			}(r)
		}
		if err != io.EOF {
			fail(err)
		}
		wg.Wait()
	}()

	return completeInfo(ctx, fail, lang, simplePages)
}

//EvolvingPage represents a wikipedia page that is being edited. Revisions is closed when there are no more revisions.
//Revision channel must be exhausted, doing otherwise may result in a deadlock.
type EvolvingPage struct {
	PageID          uint32
	Title, Abstract string
	TopicID         uint32
	Revisions       <-chan Revision
}

//Revision represents a revision of a page.
type Revision struct {
	ID, UserID uint32
	IsBot      bool
	Text, SHA1 string
	IsRevert   uint32
	Timestamp  time.Time
}

//There are 4 buffers in various forms: 4*pageBufferSize is the maximum number of wikipedia pages in memory.
//Each page has a buffer of revisionBufferSize revisions: this means that at each moment there is
//a maximum of 4*pageBufferSize*revisionBufferSize page texts in memory.
const (
	pageBufferSize     = 50
	revisionBufferSize = 250
)

func run(ctx context.Context, base bBase) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	b := base.New()
	defer b.ClosePage() //Close eventually open revision channel

	var t xml.Token
	for t, err = base.Decoder.Token(); err == nil; t, err = base.Decoder.Token() {
		switch xmlEvent(t) {
		case "page start":
			b, err = b.NewPage()
		case "id start":
			b, err = b.SetPageID(ctx, t.(xml.StartElement))
		case "revision start":
			b, err = b.NewRevision(ctx, t.(xml.StartElement))
		case "page end":
			b, err = b.ClosePage()
		}
		if err != nil {
			break
		}
	}

	causer, errHasCause := err.(interface{ Cause() error })
	switch {
	case err == io.EOF:
		err = nil
	case errHasCause && causer.Cause() != nil:
		//do nothing
	default:
		err = b.Wrapf(err, "Unexpected error in outer XML Decoder event loop")
	}

	return
}

//AnonimousUserID is the UserID value assumed by revisions done by an anonimous user
const AnonimousUserID uint32 = 0

var errInvalidXML = errors.New("Invalid XML")

type builder interface {
	NewPage() (be builder, err error)
	SetPageID(ctx context.Context, t xml.StartElement) (be builder, err error)
	NewRevision(ctx context.Context, t xml.StartElement) (be builder, err error)
	ClosePage() (be builder, err error)
	Wrapf(err error, format string, args ...interface{}) error
}

/////////////////////////////////////////////////////////////////////////////////////

//bBase is the base state builder

type bBase struct {
	Decoder         *xml.Decoder
	Article2TopicID func(articleID uint32) (topicID uint32, ok bool)
	ID2Bot          func(userID uint32) (username string, ok bool)
	OutStream       chan<- simpleEvolvingPage
	ErrorContext    *errorContext
}

type simpleEvolvingPage struct {
	PageID, TopicID uint32
	Revisions       <-chan Revision
}

func (bs *bBase) New() builder {
	be := bBase(*bs)
	return &be
}

func (bs *bBase) NewPage() (be builder, err error) {
	be = &bStarted{*bs}
	return
}
func (bs *bBase) SetPageID(ctx context.Context, t xml.StartElement) (be builder, err error) {
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (not found obligatory element \"page\" before \"id\")")
	return
}
func (bs *bBase) NewRevision(ctx context.Context, t xml.StartElement) (be builder, err error) {
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (not found obligatory element \"page\" before \"revision\")")
	return
}
func (bs *bBase) ClosePage() (be builder, err error) {
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (not found obligatory element \"page\" start before end)")
	return
}
func (bs *bBase) Wrapf(err error, format string, args ...interface{}) error {
	return errorsOnSteroids.Wrapf(err, format+" - %v", append(args, bs.ErrorContext)...)
}

/////////////////////////////////////////////////////////////////////////////////////

//bStarted is the state of the builder in which a new page start has been found
type bStarted struct {
	bBase
}

func (bs *bStarted) NewPage() (be builder, err error) { //no page nesting
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (found nested element page)")
	return
}
func (bs *bStarted) SetPageID(ctx context.Context, t xml.StartElement) (be builder, err error) {
	var pageID uint32
	if err = bs.Decoder.DecodeElement(&pageID, &t); err != nil {
		err = bs.Wrapf(err, "Error while decoding page ID")
		return
	}

	bs.ErrorContext.LastPageID = pageID //used for error reporting purposes

	if topicID, ok := bs.Article2TopicID(pageID); ok {
		revisions := make(chan Revision, revisionBufferSize)
		select {
		case <-ctx.Done():
			err = bs.Wrapf(ctx.Err(), "Context cancelled")
			return
		case bs.OutStream <- simpleEvolvingPage{pageID, topicID, revisions}:
			be = &bSetted{
				bStarted:      *bs,
				Revisions:     revisions,
				SHA12SerialID: map[string]uint32{},
			}
			return
		}
	}

	if err = bs.Decoder.Skip(); err != nil {
		err = bs.Wrapf(err, "Error while skipping page")
		return
	}

	be = bs.New()
	return
}
func (bs *bStarted) NewRevision(ctx context.Context, t xml.StartElement) (be builder, err error) { //no obligatory element "id"
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (found a page revision without finding previous page ID)")
	return
}
func (bs *bStarted) ClosePage() (be builder, err error) { //no obligatory element "id"
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (found a page end without finding previous page ID)")
	return
}
func (bs *bStarted) Wrapf(err error, format string, args ...interface{}) error {
	return errorsOnSteroids.Wrapf(err, format+" - %v", append(args, bs.ErrorContext)...)
}

/////////////////////////////////////////////////////////////////////////////////////

//bSetted is the state of the builder in which has been set a page ID for the page
type bSetted struct {
	bStarted

	Revisions     chan Revision
	RevisionCount uint32
	SHA12SerialID map[string]uint32
}

func (bs *bSetted) NewPage() (be builder, err error) { //no page nesting
	close(bs.Revisions)
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (found nested element page)")
	return
}
func (bs *bSetted) SetPageID(ctx context.Context, t xml.StartElement) (be builder, err error) {
	close(bs.Revisions)
	err = bs.Wrapf(errInvalidXML, "Error invalid xml (found a page with two ids)")
	return
}
func (bs *bSetted) NewRevision(ctx context.Context, t xml.StartElement) (be builder, err error) {
	defer func() {
		if err != nil {
			close(bs.Revisions)
		}
	}()

	//parse revision
	var r revision
	if err = bs.Decoder.DecodeElement(&r, &t); err != nil {
		err = bs.Wrapf(err, "Error while decoding the %vth revision", bs.RevisionCount+1)
		return
	}

	//Calculate reverts
	serialID, IsRevert := bs.RevisionCount, uint32(0)
	oldSerialID, isRevert := bs.SHA12SerialID[r.SHA1]
	switch {
	case isRevert:
		IsRevert = serialID - (oldSerialID + 1)
		fallthrough
	case len(r.SHA1) == 31:
		bs.SHA12SerialID[r.SHA1] = serialID
	}

	//convert time
	const layout = "2006-01-02T15:04:05Z"
	timestamp, err := time.Parse(layout, r.Timestamp)
	if err != nil {
		err = bs.Wrapf(err, "Error while decoding the timestamp %s of %vth revision", r.Timestamp, bs.RevisionCount+1)
		return
	}
	r.Timestamp = ""

	//Check if userID represents bot
	_, isBot := bs.ID2Bot(r.UserID)

	bs.RevisionCount++

	select {
	case <-ctx.Done():
		err = bs.Wrapf(ctx.Err(), "Context cancelled")
	case bs.Revisions <- Revision{r.ID, r.UserID, isBot, r.Text, r.SHA1, IsRevert, timestamp}:
		be = bs
	}

	return
}
func (bs *bSetted) ClosePage() (be builder, err error) {
	close(bs.Revisions)
	be = bs.New()
	return
}

// A page revision.
type revision struct {
	ID        uint32 `xml:"id"`
	Timestamp string `xml:"timestamp"`
	UserID    uint32 `xml:"contributor>id"`
	Text      string `xml:"text"`
	SHA1      string `xml:"sha1"`
	//converted data
	timestamp time.Time
}

func xmlEvent(t xml.Token) string {
	switch elem := t.(type) {
	case xml.StartElement:
		return elem.Name.Local + " start"
	case xml.EndElement:
		return elem.Name.Local + " end"
	default:
		return ""
	}
}

type errorContext struct {
	LastPageID uint32 //used for error reporting purposes
	Filename   string //used for error reporting purposes
}

func (ec errorContext) String() string {
	report := fmt.Sprintf("last page ID %v in \"%s\"", ec.LastPageID, ec.Filename)
	if _, err := os.Stat(ec.Filename); os.IsNotExist(err) {
		report += " - WARNING: file not found!"
	}
	return report
}

func filename(r io.Reader) (filename string) {
	if namer, ok := r.(interface{ Name() string }); ok {
		filename = namer.Name()
	}
	return
}

func getArticle2TopicID(ctx context.Context, tmpDir, lang string) (article2TopicID func(uint32) (uint32, bool), err error) {
	article2Topic, namespaces, err := wikiassignment.From(ctx, tmpDir, lang)
	if err != nil {
		return
	}

	//Filter out non articles
	articlesIDS := roaring.BitmapOf(namespaces.Articles...)
	for pageID := range article2Topic {
		if !articlesIDS.Contains(pageID) {
			delete(article2Topic, pageID)
		}
	}

	return func(articleID uint32) (topicID uint32, ok bool) {
		topicID, ok = article2Topic[articleID]
		return
	}, nil
}

func completeInfo(ctx context.Context, fail func(err error) error, lang string, pages <-chan simpleEvolvingPage) <-chan EvolvingPage {
	results := make(chan EvolvingPage, pageBufferSize)
	go func() {
		defer close(results)
		wikiPage := wikipage.New(lang)
		wg := sync.WaitGroup{}
		for i := 0; i < pageBufferSize; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
			loop:
				for p := range pages {
					wp, err := wikiPage.From(ctx, p.PageID) //bottle neck: query to wikipedia api for each page
					_, NotFound := wikipage.NotFound(err)
					switch {
					case NotFound:
						for range p.Revisions {
							//Empty revision channel using the same goroutine:
							//if some error arises is caught by fail
						}
						continue loop
					case err != nil:
						fail(err)
						return
					}

					select {
					case results <- EvolvingPage{p.PageID, wp.Title, wp.Abstract, p.TopicID, p.Revisions}:
						//proceed
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		wg.Wait()
	}()

	return results
}
