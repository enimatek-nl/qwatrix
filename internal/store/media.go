package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// MediaFile is one uploaded file's metadata.
//
// The bytes live on disk and the row is the index. That split is deliberate: a file can be tens of
// megabytes and is served as a stream, while the metadata is small, frequently queried on its own
// (does this ID exist? what content type? what filename did the uploader give?), and has to survive
// alongside the database it describes. Two copies of the truth would be a leak waiting to happen, so
// a row is written only once its bytes are safely on disk and deleted together with them.
type MediaFile struct {
	// ID is the media ID: the last segment of the mxc:// URI. It is opaque and server-wide, so
	// two servers never have to guess whose "4" a mxc URI means.
	ID string `gorm:"primaryKey;size:255" json:"media_id"`
	// Origin is the server that holds this file. It is stored because a download request names
	// the origin it believes it is talking to, and answering for somebody else's media ID would
	// let one server's cache impersonate another's content.
	Origin    string `gorm:"size:255;index" json:"-"`
	Localpart string `gorm:"size:255;index" json:"-"`
	// ContentType is served back verbatim. It is what the uploader sent, because the uploader is
	// the party that knew what the bytes were; a sniffed type would be a guess about content the
	// server has no business reinterpreting — and it is the difference between a client rendering
	// an image and offering to download one.
	ContentType string `gorm:"size:255" json:"-"`
	// FileName is the uploader's suggested name, echoed in Content-Disposition.
	FileName string `gorm:"size:1024" json:"-"`
	// Size and SHA256 are recorded at upload so a download can be verified, logged, and bounded
	// without re-reading the file.
	SHA256 string `gorm:"size:64" json:"-"`
	Size   int64  `json:"-"`
	// CreatedMS is milliseconds since the epoch, for retention and for a future quarantine of
	// media that predates a moderation decision.
	CreatedMS int64 `gorm:"index" json:"-"`
}

// TableName pins the table name.
func (MediaFile) TableName() string { return "media_files" }

// MediaRepo is the media metadata repository.
type MediaRepo interface {
	// Put stores metadata for a file whose bytes are already on disk.
	Put(ctx context.Context, file *MediaFile) error
	// Get returns one row, or [ErrNotFound].
	Get(ctx context.Context, id string) (*MediaFile, error)
	// Delete removes the metadata of one file.
	Delete(ctx context.Context, id string) error
	// Count returns how many files this server holds.
	Count(ctx context.Context) (int64, error)
}

// MediaRepoImpl is the GORM-backed [MediaRepo].
type MediaRepoImpl struct{ db *gorm.DB }

// NewMediaRepo builds the media metadata repository.
func NewMediaRepo(db *gorm.DB) *MediaRepoImpl { return &MediaRepoImpl{db: db} }

// Put implements [MediaRepo].
func (r *MediaRepoImpl) Put(ctx context.Context, file *MediaFile) error {
	if err := session(ctx, r.db).Save(file).Error; err != nil {
		return fmt.Errorf("storing media metadata %q: %w", file.ID, err)
	}
	return nil
}

// Get implements [MediaRepo].
func (r *MediaRepoImpl) Get(ctx context.Context, id string) (*MediaFile, error) {
	var row MediaFile
	if err := session(ctx, r.db).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("media %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("reading media %q: %w", id, err)
	}
	return &row, nil
}

// Delete implements [MediaRepo].
func (r *MediaRepoImpl) Delete(ctx context.Context, id string) error {
	if err := session(ctx, r.db).Where("id = ?", id).Delete(&MediaFile{}).Error; err != nil {
		return fmt.Errorf("deleting media %q: %w", id, err)
	}
	return nil
}

// Count implements [MediaRepo].
func (r *MediaRepoImpl) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := session(ctx, r.db).Model(&MediaFile{}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting media: %w", err)
	}
	return count, nil
}
