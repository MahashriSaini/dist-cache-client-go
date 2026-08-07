// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dcache

import (
	"fmt"
	"hash/crc32"
	"strconv"
)

const checksumMetadataKey = "CHUNK_CHECKSUM"

func metadataWithChecksum(metadata map[string][]byte, data []byte) map[string][]byte {
	result := make(map[string][]byte, len(metadata)+1)
	for key, value := range metadata {
		result[key] = value
	}
	result[checksumMetadataKey] = []byte(strconv.FormatUint(uint64(crc32.ChecksumIEEE(data)), 10))
	return result
}

func validateChecksum(data []byte, metadata map[string][]byte) error {
	receivedValue, ok := metadata[checksumMetadataKey]
	if !ok {
		return fmt.Errorf("%w: missing checksum metadata", ErrChecksumMismatch)
	}
	received, err := strconv.ParseUint(string(receivedValue), 10, 32)
	if err != nil {
		return fmt.Errorf("%w: invalid checksum metadata", ErrChecksumMismatch)
	}
	calculated := crc32.ChecksumIEEE(data)
	if uint32(received) != calculated {
		return fmt.Errorf("%w: expected %d, calculated %d", ErrChecksumMismatch, received, calculated)
	}
	return nil
}
