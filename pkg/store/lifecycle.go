package store

import "context"

type ErasePreview struct {
	DatasetID          string `json:"dataset_id"`
	ObjectCount        int    `json:"object_count"`
	TypeCount          int    `json:"type_count"`
	AffectedPrincipals int    `json:"affected_principals"`
	Watermark          string `json:"watermark"`
}
type EraseResult struct {
	DatasetID string `json:"dataset_id"`
	Watermark string `json:"watermark"`
}
type Lifecycle interface {
	PreviewErase(context.Context, Access, string) (ErasePreview, error)
	Erase(context.Context, Access, string, string) (EraseResult, error)
}
