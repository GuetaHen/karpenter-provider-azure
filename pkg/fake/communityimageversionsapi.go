/*
Portions Copyright (c) Microsoft Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fake

import (
	"context"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/samber/lo"

	imagefamilytypes "github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily/types"
)

// TODO: no ability to simulate errors on the Pager.
type CommunityGalleryImageVersionsAPI struct {
	ImageVersions AtomicPtrSlice[armcompute.CommunityGalleryImageVersion]
	// NotFoundImageDefinitions names image definitions whose version listing fails with not found, like an
	// image definition that the location does not publish.
	NotFoundImageDefinitions []string
}

// assert that the fake implements the interface
var _ imagefamilytypes.CommunityGalleryImageVersionsAPI = &CommunityGalleryImageVersionsAPI{}

// NewListPager returns a new pager to return the next page of CommunityGalleryImageVersionsClientListResponse
func (c *CommunityGalleryImageVersionsAPI) NewListPager(_ string, _ string, galleryImageName string, _ *armcompute.CommunityGalleryImageVersionsClientListOptions) *runtime.Pager[armcompute.CommunityGalleryImageVersionsClientListResponse] {
	notFound := lo.Contains(c.NotFoundImageDefinitions, galleryImageName)
	pagingHandler := runtime.PagingHandler[armcompute.CommunityGalleryImageVersionsClientListResponse]{
		More: func(page armcompute.CommunityGalleryImageVersionsClientListResponse) bool {
			return false
		},
		Fetcher: func(ctx context.Context, _ *armcompute.CommunityGalleryImageVersionsClientListResponse) (armcompute.CommunityGalleryImageVersionsClientListResponse, error) {
			if notFound {
				return armcompute.CommunityGalleryImageVersionsClientListResponse{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceNotFound"}
			}
			output := armcompute.CommunityGalleryImageVersionList{
				Value: []*armcompute.CommunityGalleryImageVersion{},
			}
			output.Value = append(output.Value, c.ImageVersions.values...)
			return armcompute.CommunityGalleryImageVersionsClientListResponse{
				CommunityGalleryImageVersionList: output,
			}, nil
		},
	}
	return runtime.NewPager(pagingHandler)
}

func (c *CommunityGalleryImageVersionsAPI) Reset() {
	if c == nil {
		return
	}
	c.ImageVersions.Reset()
	c.NotFoundImageDefinitions = nil
}
