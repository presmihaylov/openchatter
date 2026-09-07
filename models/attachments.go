package models

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/presmihaylov/openchatter/pkg/imgvariant"
)

// roomStorageCap bounds total attachment bytes per room so one participant
// can't fill the shared database disk.
const roomStorageCap = 500 * 1024 * 1024

var ErrQuota = errors.New("room storage quota exceeded")

func (s *Store) CreateAttachment(ctx context.Context, roomID, uploaderID, filename, contentType string, data []byte) (AttachmentMeta, error) {
	var meta AttachmentMeta
	err := s.pool.QueryRow(ctx,
		`INSERT INTO attachments (room_id, uploader_id, filename, content_type, size_bytes, data)
		 SELECT $1::uuid, $2::uuid, $3::text, $4::text, $5::bigint, $6::bytea
		 WHERE (SELECT COALESCE(sum(size_bytes), 0) FROM attachments WHERE room_id = $1) + $5 <= $7
		 RETURNING id, filename, content_type, size_bytes, created_at`,
		roomID, uploaderID, filename, contentType, len(data), data, roomStorageCap,
	).Scan(&meta.ID, &meta.Filename, &meta.ContentType, &meta.SizeBytes, &meta.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return meta, fmt.Errorf("%w (%dMB per room)", ErrQuota, roomStorageCap/(1024*1024))
	}
	return meta, err
}

func (s *Store) AttachmentByID(ctx context.Context, roomID, id string) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx,
		`SELECT id, filename, content_type, size_bytes, created_at, room_id, uploader_id, data
		 FROM attachments WHERE room_id = $1 AND id = $2`, roomID, id,
	).Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt, &a.RoomID, &a.UploaderID, &a.Data)
	return a, mapRowErr(err)
}

// DeleteOrphanAttachments removes uploads over a day old that no message
// references; 24h leaves plenty of slack between upload and post.
func (s *Store) DeleteOrphanAttachments(ctx context.Context) (int64, error) {
	res, err := s.pool.Exec(ctx,
		`DELETE FROM attachments a
		 WHERE a.created_at < now() - interval '24 hours'
		   AND NOT EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.attachment_id = a.id)
		   AND NOT EXISTS (SELECT 1 FROM participants pt WHERE pt.avatar_attachment_id = a.id)
		   AND NOT EXISTS (SELECT 1 FROM rooms rm WHERE rm.avatar_attachment_id = a.id)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// AttachmentVariant is one of the small copies an image upload gets on the
// way in (pkg/imgvariant): 128px for every avatar and logo drawn in the
// chrome, 512px for the profile and settings views. The original stays.
const (
	VariantSmall = 128
	VariantLarge = 512
)

// AttachmentSized returns the variant at size when the upload has one, else
// the original: a non-image, an unsupported format, or a size of 0.
func (s *Store) AttachmentSized(ctx context.Context, roomID, id string, size int) (Attachment, error) {
	col := ""
	switch size {
	case VariantSmall:
		col = "variant_128"
	case VariantLarge:
		col = "variant_512"
	default:
		return s.AttachmentByID(ctx, roomID, id)
	}
	var a Attachment
	err := s.pool.QueryRow(ctx,
		`SELECT id, filename, COALESCE(CASE WHEN `+col+` IS NULL THEN NULL ELSE variant_type END, content_type),
		        size_bytes, created_at, room_id, uploader_id, COALESCE(`+col+`, data)
		 FROM attachments WHERE room_id = $1 AND id = $2`, roomID, id,
	).Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt, &a.RoomID, &a.UploaderID, &a.Data)
	a.SizeBytes = int64(len(a.Data))
	return a, mapRowErr(err)
}

// SetAttachmentVariants stores the resized copies; a nil pair with a type
// marks an upload that cannot be resized so the backfill stops retrying it.
func (s *Store) SetAttachmentVariants(ctx context.Context, id string, small, large []byte, contentType string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE attachments SET variant_128 = $2, variant_512 = $3, variant_type = $4 WHERE id = $1`,
		id, small, large, contentType)
	return err
}

// attachmentAnyRoom is the backfill's room-less read; callers that serve
// bytes to a user go through AttachmentByID with the room scope.
func (s *Store) attachmentAnyRoom(ctx context.Context, id string) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx,
		`SELECT id, filename, content_type, size_bytes, created_at, room_id, uploader_id, data
		 FROM attachments WHERE id = $1`, id,
	).Scan(&a.ID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedAt, &a.RoomID, &a.UploaderID, &a.Data)
	return a, mapRowErr(err)
}

// AvatarsWithoutVariants lists the ids of image uploads some participant or
// room shows as its avatar that were never resized (variant_type IS NULL).
// Ids only: the backfill loads one body at a time to keep boot memory flat.
func (s *Store) AvatarsWithoutVariants(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM attachments a
		 WHERE a.variant_type IS NULL AND a.content_type LIKE 'image/%'
		   AND (EXISTS (SELECT 1 FROM participants pt WHERE pt.avatar_attachment_id = a.id)
		     OR EXISTS (SELECT 1 FROM rooms rm WHERE rm.avatar_attachment_id = a.id))
		 ORDER BY a.size_bytes DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BackfillAvatarVariants resizes every avatar and logo uploaded before
// variants existed. Idempotent: a second run finds nothing. Returns how many
// got variants and how many were marked unsupported.
func (s *Store) BackfillAvatarVariants(ctx context.Context) (int, int, error) {
	list, err := s.AvatarsWithoutVariants(ctx)
	if err != nil {
		return 0, 0, err
	}
	done, skipped := 0, 0
	for _, id := range list {
		a, err := s.attachmentAnyRoom(ctx, id)
		if err != nil {
			return done, skipped, err
		}
		v, err := imgvariant.Make(a.Data, a.ContentType)
		if err != nil {
			skipped++
			if err := s.SetAttachmentVariants(ctx, a.ID, nil, nil, "none"); err != nil {
				return done, skipped, err
			}
			continue
		}
		if err := s.SetAttachmentVariants(ctx, a.ID, v.Small, v.Large, v.ContentType); err != nil {
			return done, skipped, err
		}
		done++
	}
	return done, skipped, nil
}
