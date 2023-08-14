// Copyright 2023 Buf Technologies, Inc.
//
// All rights reserved.

package vanguard

import (
	"compress/gzip"
	"sync"

	"connectrpc.com/connect"
)

// DefaultGzipCompressor is a factory for Compressor instances used by default
// for the "gzip" encoding type.
func DefaultGzipCompressor() connect.Compressor {
	return &gzip.Writer{}
}

// DefaultGzipDecompressor is a factory for Decompressor instances used by
// default for the "gzip" encoding type.
func DefaultGzipDecompressor() connect.Decompressor {
	return &gzip.Reader{}
}

type compressionPool struct {
	name          string
	decompressors sync.Pool
	compressors   sync.Pool
}

func newCompressionPool(
	name string,
	newCompressor func() connect.Compressor,
	newDecompressor func() connect.Decompressor,
) *compressionPool {
	return &compressionPool{
		name: name,
		compressors: sync.Pool{
			New: func() any { return newCompressor() },
		},
		decompressors: sync.Pool{
			New: func() any { return newDecompressor() },
		},
	}
}

func (p *compressionPool) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

//nolint:unused // temporary
func (p *compressionPool) getCompressor() connect.Compressor {
	return p.compressors.Get().(connect.Compressor) //nolint:forcetypeassert
}

func (p *compressionPool) getDecompressor() connect.Decompressor {
	return p.decompressors.Get().(connect.Decompressor) //nolint:forcetypeassert
}
