package server

import (
	"context"
	"os"

	pb "api/internal/adapters/framework/left/server/grpc/pb"
)

type gRPCServer struct {
	pb pb.UnimplementedFileTransferServiceServer
}

func (s *gRPCServer) UploadFile(ctx context.Context, in *pb.UploadFileRequest) (*pb.UploadFileResponse, error) {
	data := in.GetData()
	err := os.WriteFile("received_file", data, 0644)
	if err != nil {
		return &pb.UploadFileResponse{Success: false, Message: "Failed to save file"}, err
	}
	return &pb.UploadFileResponse{Success: true, Message: "File saved successfully"}, nil
}

func (s *gRPCServer) DownloadFile(ctx context.Context, in *pb.DownloadFileRequest) (*pb.FileChunk, error) {
	filename := in.GetFilename()
	data, err := os.ReadFile(filename)
	if err != nil {
		return &pb.FileChunk{}, err
	}
	return &pb.FileChunk{Data: data}, nil
}
