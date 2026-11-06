package server

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
)

func TestConversionHelpers_Nil(t *testing.T) {
	if beadToProto(nil) != nil {
		t.Error("beadToProto(nil) should be nil")
	}
	if dependencyToProto(nil) != nil {
		t.Error("dependencyToProto(nil) should be nil")
	}
	if commentToProto(nil) != nil {
		t.Error("commentToProto(nil) should be nil")
	}
	if eventToProto(nil) != nil {
		t.Error("eventToProto(nil) should be nil")
	}
	if configToProto(nil) != nil {
		t.Error("configToProto(nil) should be nil")
	}
	if protoTimestamp(nil) != nil {
		t.Error("protoTimestamp(nil) should be nil")
	}
	if storeError(nil, "bead") != nil {
		t.Error("storeError(nil) should be nil")
	}
}

func TestStoreError_InternalError(t *testing.T) {
	err := storeError(fmt.Errorf("something went wrong"), "bead")
	requireCode(t, err, codes.Internal)
}
