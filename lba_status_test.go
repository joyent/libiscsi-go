package iscsi

import (
	"testing"

	"gotest.tools/assert"
)

func TestParseLBAStatusData(t *testing.T) {
	testCases := []struct {
		desc     string
		data     []byte
		expected []LBAStatusDescriptor
	}{
		{
			desc:     "too short to contain a header",
			data:     []byte{0, 0, 0},
			expected: nil,
		},
		{
			desc:     "header only, no descriptors",
			data:     []byte{0, 0, 0, 4, 0, 0, 0, 0},
			expected: nil,
		},
		{
			desc: "single mapped descriptor",
			data: []byte{
				0, 0, 0, 20, // parameter data length
				0, 0, 0, 0, // reserved
				0, 0, 0, 0, 0, 0, 0, 10, // lba 10
				0, 0, 0, 5, // 5 blocks
				0x00, 0, 0, 0, // mapped
			},
			expected: []LBAStatusDescriptor{
				{LBA: 10, NumBlocks: 5, Provisioning: ProvisioningStatusMapped},
			},
		},
		{
			desc: "two descriptors, second deallocated",
			data: []byte{
				0, 0, 0, 36, // parameter data length
				0, 0, 0, 0, // reserved
				0, 0, 0, 0, 0, 0, 0, 0, // lba 0
				0, 0, 0, 100, // 100 blocks
				0x00, 0, 0, 0, // mapped
				0, 0, 0, 0, 0, 0, 0, 100, // lba 100
				0, 0, 3, 0xe8, // 1000 blocks
				0x01, 0, 0, 0, // deallocated
			},
			expected: []LBAStatusDescriptor{
				{LBA: 0, NumBlocks: 100, Provisioning: ProvisioningStatusMapped},
				{LBA: 100, NumBlocks: 1000, Provisioning: ProvisioningStatusDeallocated},
			},
		},
		{
			desc: "parameter data length truncates trailing garbage",
			data: []byte{
				0, 0, 0, 20, // parameter data length covers only 1 descriptor
				0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 1,
				0x02, 0, 0, 0, // anchored
				// a second, malformed/partial descriptor that should be ignored
				0, 0, 0, 0, 0, 0, 0, 1,
			},
			expected: []LBAStatusDescriptor{
				{LBA: 0, NumBlocks: 1, Provisioning: ProvisioningStatusAnchored},
			},
		},
	}

	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			assert.DeepEqual(t, parseLBAStatusData(tC.data), tC.expected)
		})
	}
}
