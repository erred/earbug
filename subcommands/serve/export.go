package serve

import (
	"bytes"
	"context"
	"io"

	"github.com/bufbuild/connect-go"
	"github.com/klauspost/compress/zstd"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"gocloud.dev/blob"
	"google.golang.org/protobuf/proto"
)

func (s *Server) Export(ctx context.Context, r *connect.Request[earbugv4.ExportRequest]) (*connect.Response[earbugv4.ExportResponse], error) {
	ctx, span := s.o.T.Start(ctx, "Export")
	defer span.End()

	s.storemu.Lock()
	b, err := proto.Marshal(&s.store)
	s.storemu.Unlock()

	if err != nil {
		return nil, s.o.markErr(ctx, "marshal store", err)
	}

	res := &connect.Response[earbugv4.ExportResponse]{Msg: &earbugv4.ExportResponse{}}
	if r.Msg.Bucket == "" {
		res.Msg.Content = b
	} else {
		bkt, err := blob.OpenBucket(ctx, r.Msg.Bucket)
		if err != nil {
			return nil, s.o.markErr(ctx, "open destination bucket", err)
		}

		ow, err := bkt.NewWriter(ctx, r.Msg.Key, nil)
		if err != nil {
			return nil, s.o.markErr(ctx, "open destination key", err)
		}
		defer ow.Close()
		zw, err := zstd.NewWriter(ow)
		if err != nil {
			return nil, s.o.markErr(ctx, "new zstd writer", err)
		}
		defer zw.Close()
		_, err = io.Copy(zw, bytes.NewReader(b))
		if err != nil {
			return nil, s.o.markErr(ctx, "write store", err)
		}
	}
	return res, nil
}
