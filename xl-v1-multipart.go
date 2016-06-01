/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio/pkg/mimedb"
)

// ListMultipartUploads - list multipart uploads.
func (xl xlObjects) ListMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (ListMultipartsInfo, error) {
	return xl.listMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker, delimiter, maxUploads)
}

// newMultipartUpload - initialize a new multipart.
func (xl xlObjects) newMultipartUpload(bucket string, object string, meta map[string]string) (uploadID string, err error) {
	// Verify if bucket name is valid.
	if !IsValidBucketName(bucket) {
		return "", BucketNameInvalid{Bucket: bucket}
	}
	// Verify whether the bucket exists.
	if !xl.isBucketExist(bucket) {
		return "", BucketNotFound{Bucket: bucket}
	}
	// Verify if object name is valid.
	if !IsValidObjectName(object) {
		return "", ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	// No metadata is set, allocate a new one.
	if meta == nil {
		meta = make(map[string]string)
	}

	xlMeta := newXLMetaV1(xl.dataBlocks, xl.parityBlocks)
	// If not set default to "application/octet-stream"
	if meta["content-type"] == "" {
		contentType := "application/octet-stream"
		if objectExt := filepath.Ext(object); objectExt != "" {
			content, ok := mimedb.DB[strings.ToLower(strings.TrimPrefix(objectExt, "."))]
			if ok {
				contentType = content.ContentType
			}
		}
		meta["content-type"] = contentType
	}
	xlMeta.Stat.ModTime = time.Now().UTC()
	xlMeta.Stat.Version = 1
	xlMeta.Meta = meta

	// This lock needs to be held for any changes to the directory contents of ".minio/multipart/object/"
	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))

	uploadID = getUUID()
	initiated := time.Now().UTC()
	// Create 'uploads.json'
	if err = writeUploadJSON(bucket, object, uploadID, initiated, xl.storageDisks...); err != nil {
		return "", err
	}
	uploadIDPath := path.Join(mpartMetaPrefix, bucket, object, uploadID)
	tempUploadIDPath := path.Join(tmpMetaPrefix, uploadID)
	if err = xl.writeXLMetadata(minioMetaBucket, tempUploadIDPath, xlMeta); err != nil {
		return "", toObjectErr(err, minioMetaBucket, tempUploadIDPath)
	}
	rErr := xl.renameObject(minioMetaBucket, tempUploadIDPath, minioMetaBucket, uploadIDPath)
	if rErr == nil {
		// Return success.
		return uploadID, nil
	}
	return "", toObjectErr(rErr, minioMetaBucket, uploadIDPath)
}

// NewMultipartUpload - initialize a new multipart upload, returns a unique id.
func (xl xlObjects) NewMultipartUpload(bucket, object string, meta map[string]string) (string, error) {
	return xl.newMultipartUpload(bucket, object, meta)
}

// putObjectPart - put object part.
func (xl xlObjects) putObjectPart(bucket string, object string, uploadID string, partID int, size int64, data io.Reader, md5Hex string) (string, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return "", BucketNameInvalid{Bucket: bucket}
	}
	// Verify whether the bucket exists.
	if !xl.isBucketExist(bucket) {
		return "", BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return "", ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	uploadIDPath := pathJoin(mpartMetaPrefix, bucket, object, uploadID)
	nsMutex.Lock(minioMetaBucket, uploadIDPath)
	defer nsMutex.Unlock(minioMetaBucket, uploadIDPath)

	if !xl.isUploadIDExists(bucket, object, uploadID) {
		return "", InvalidUploadID{UploadID: uploadID}
	}

	// Read metadata associated with the object from all disks.
	partsMetadata, errs := xl.readAllXLMetadata(minioMetaBucket, uploadIDPath)

	// List all online disks.
	onlineDisks, higherVersion, err := xl.listOnlineDisks(partsMetadata, errs)
	if err != nil {
		return "", toObjectErr(err, bucket, object)
	}

	// Pick one from the first valid metadata.
	xlMeta := pickValidXLMeta(partsMetadata)

	// Initialize a new erasure with online disks and new distribution.
	erasure := newErasure(onlineDisks, xlMeta.Erasure.Distribution)

	// Initialize sha512 hash.
	erasure.InitHash("sha512")

	partSuffix := fmt.Sprintf("object%d", partID)
	tmpPartPath := path.Join(tmpMetaPrefix, uploadID, partSuffix)

	// Initialize md5 writer.
	md5Writer := md5.New()

	// Allocate blocksized buffer for reading.
	buf := make([]byte, blockSizeV1)

	// Read until io.EOF, fill the allocated buf.
	for {
		var n int
		n, err = io.ReadFull(data, buf)
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return "", toObjectErr(err, bucket, object)
		}
		// Update md5 writer.
		md5Writer.Write(buf[:n])
		var m int64
		m, err = erasure.AppendFile(minioMetaBucket, tmpPartPath, buf[:n])
		if err != nil {
			return "", toObjectErr(err, minioMetaBucket, tmpPartPath)
		}
		if m != int64(len(buf[:n])) {
			return "", toObjectErr(errUnexpected, bucket, object)
		}
	}

	// Calculate new md5sum.
	newMD5Hex := hex.EncodeToString(md5Writer.Sum(nil))
	if md5Hex != "" {
		if newMD5Hex != md5Hex {
			return "", BadDigest{md5Hex, newMD5Hex}
		}
	}

	if !xl.isUploadIDExists(bucket, object, uploadID) {
		return "", InvalidUploadID{UploadID: uploadID}
	}

	// Rename temporary part file to its final location.
	partPath := path.Join(uploadIDPath, partSuffix)
	err = xl.renameObject(minioMetaBucket, tmpPartPath, minioMetaBucket, partPath)
	if err != nil {
		return "", toObjectErr(err, minioMetaBucket, partPath)
	}

	// Once part is successfully committed, proceed with updating XL metadata.
	xlMeta.Stat.Version = higherVersion
	// Add the current part.
	xlMeta.AddObjectPart(partID, partSuffix, newMD5Hex, size)

	// Get calculated hash checksums from erasure to save in `xl.json`.
	hashChecksums := erasure.GetHashes()

	checkSums := make([]checkSumInfo, len(xl.storageDisks))
	for index := range xl.storageDisks {
		blockIndex := xlMeta.Erasure.Distribution[index] - 1
		checkSums[blockIndex] = checkSumInfo{
			Name:      partSuffix,
			Algorithm: "sha512",
			Hash:      hashChecksums[blockIndex],
		}
	}
	for index := range partsMetadata {
		blockIndex := xlMeta.Erasure.Distribution[index] - 1
		partsMetadata[index].Parts = xlMeta.Parts
		partsMetadata[index].Erasure.Checksum = append(partsMetadata[index].Erasure.Checksum, checkSums[blockIndex])
	}

	// Write all the checksum metadata.
	tempUploadIDPath := path.Join(tmpMetaPrefix, uploadID)

	// Write unique `xl.json` each disk.
	if err = xl.writeUniqueXLMetadata(minioMetaBucket, tempUploadIDPath, partsMetadata); err != nil {
		return "", toObjectErr(err, minioMetaBucket, tempUploadIDPath)
	}
	rErr := xl.renameXLMetadata(minioMetaBucket, tempUploadIDPath, minioMetaBucket, uploadIDPath)
	if rErr != nil {
		return "", toObjectErr(rErr, minioMetaBucket, uploadIDPath)
	}

	// Return success.
	return newMD5Hex, nil
}

// PutObjectPart - writes the multipart upload chunks.
func (xl xlObjects) PutObjectPart(bucket, object, uploadID string, partID int, size int64, data io.Reader, md5Hex string) (string, error) {
	return xl.putObjectPart(bucket, object, uploadID, partID, size, data, md5Hex)
}

// ListObjectParts - list object parts.
func (xl xlObjects) listObjectParts(bucket, object, uploadID string, partNumberMarker, maxParts int) (ListPartsInfo, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return ListPartsInfo{}, BucketNameInvalid{Bucket: bucket}
	}
	// Verify whether the bucket exists.
	if !xl.isBucketExist(bucket) {
		return ListPartsInfo{}, BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return ListPartsInfo{}, ObjectNameInvalid{Bucket: bucket, Object: object}
	}
	// Hold lock so that there is no competing abort-multipart-upload or complete-multipart-upload.
	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))

	if !xl.isUploadIDExists(bucket, object, uploadID) {
		return ListPartsInfo{}, InvalidUploadID{UploadID: uploadID}
	}

	result := ListPartsInfo{}

	uploadIDPath := path.Join(mpartMetaPrefix, bucket, object, uploadID)

	xlMeta, err := xl.readXLMetadata(minioMetaBucket, uploadIDPath)
	if err != nil {
		return ListPartsInfo{}, toObjectErr(err, minioMetaBucket, uploadIDPath)
	}

	// Populate the result stub.
	result.Bucket = bucket
	result.Object = object
	result.UploadID = uploadID
	result.MaxParts = maxParts

	// For empty number of parts or maxParts as zero, return right here.
	if len(xlMeta.Parts) == 0 || maxParts == 0 {
		return result, nil
	}

	// Limit output to maxPartsList.
	if maxParts > maxPartsList {
		maxParts = maxPartsList
	}

	// Only parts with higher part numbers will be listed.
	partIdx := xlMeta.ObjectPartIndex(partNumberMarker)
	parts := xlMeta.Parts
	if partIdx != -1 {
		parts = xlMeta.Parts[partIdx+1:]
	}
	count := maxParts
	for _, part := range parts {
		partNamePath := path.Join(mpartMetaPrefix, bucket, object, uploadID, part.Name)
		var fi FileInfo
		fi, err = xl.statPart(minioMetaBucket, partNamePath)
		if err != nil {
			return ListPartsInfo{}, toObjectErr(err, minioMetaBucket, partNamePath)
		}
		result.Parts = append(result.Parts, partInfo{
			PartNumber:   part.Number,
			ETag:         part.ETag,
			LastModified: fi.ModTime,
			Size:         part.Size,
		})
		count--
		if count == 0 {
			break
		}
	}
	// If listed entries are more than maxParts, we set IsTruncated as true.
	if len(parts) > len(result.Parts) {
		result.IsTruncated = true
		// Make sure to fill next part number marker if IsTruncated is
		// true for subsequent listing.
		nextPartNumberMarker := result.Parts[len(result.Parts)-1].PartNumber
		result.NextPartNumberMarker = nextPartNumberMarker
	}
	return result, nil
}

// ListObjectParts - list object parts.
func (xl xlObjects) ListObjectParts(bucket, object, uploadID string, partNumberMarker, maxParts int) (ListPartsInfo, error) {
	return xl.listObjectParts(bucket, object, uploadID, partNumberMarker, maxParts)
}

func (xl xlObjects) CompleteMultipartUpload(bucket string, object string, uploadID string, parts []completePart) (string, error) {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return "", BucketNameInvalid{Bucket: bucket}
	}
	// Verify whether the bucket exists.
	if !xl.isBucketExist(bucket) {
		return "", BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return "", ObjectNameInvalid{
			Bucket: bucket,
			Object: object,
		}
	}
	// Hold lock so that
	// 1) no one aborts this multipart upload
	// 2) no one does a parallel complete-multipart-upload on this multipart upload
	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))

	if !xl.isUploadIDExists(bucket, object, uploadID) {
		return "", InvalidUploadID{UploadID: uploadID}
	}
	// Calculate s3 compatible md5sum for complete multipart.
	s3MD5, err := completeMultipartMD5(parts...)
	if err != nil {
		return "", err
	}

	uploadIDPath := pathJoin(mpartMetaPrefix, bucket, object, uploadID)

	// Read metadata associated with the object from all disks.
	partsMetadata, errs := xl.readAllXLMetadata(minioMetaBucket, uploadIDPath)
	if err = xl.reduceError(errs); err != nil {
		return "", toObjectErr(err, minioMetaBucket, uploadIDPath)
	}

	// Calculate full object size.
	var objectSize int64

	// Pick one from the first valid metadata.
	xlMeta := pickValidXLMeta(partsMetadata)

	// Save current xl meta for validation.
	var currentXLMeta = xlMeta

	// Allocate parts similar to incoming slice.
	xlMeta.Parts = make([]objectPartInfo, len(parts))

	// Loop through all parts, validate them and then commit to disk.
	for i, part := range parts {
		partIdx := currentXLMeta.ObjectPartIndex(part.PartNumber)
		if partIdx == -1 {
			return "", InvalidPart{}
		}
		if currentXLMeta.Parts[partIdx].ETag != part.ETag {
			return "", BadDigest{}
		}
		// All parts except the last part has to be atleast 5MB.
		if (i < len(parts)-1) && !isMinAllowedPartSize(currentXLMeta.Parts[partIdx].Size) {
			return "", PartTooSmall{}
		}

		// Save for total object size.
		objectSize += currentXLMeta.Parts[partIdx].Size

		// Add incoming parts.
		xlMeta.Parts[i] = objectPartInfo{
			Number: part.PartNumber,
			ETag:   part.ETag,
			Size:   currentXLMeta.Parts[partIdx].Size,
			Name:   fmt.Sprintf("object%d", part.PartNumber),
		}
	}

	// Check if an object is present as one of the parent dir.
	if xl.parentDirIsObject(bucket, path.Dir(object)) {
		return "", toObjectErr(errFileAccessDenied, bucket, object)
	}

	// Save the final object size and modtime.
	xlMeta.Stat.Size = objectSize
	xlMeta.Stat.ModTime = time.Now().UTC()

	// Save successfully calculated md5sum.
	xlMeta.Meta["md5Sum"] = s3MD5
	uploadIDPath = path.Join(mpartMetaPrefix, bucket, object, uploadID)
	tempUploadIDPath := path.Join(tmpMetaPrefix, uploadID)

	// Update all xl metadata, make sure to not modify fields like
	// checksum which are different on each disks.
	for index := range partsMetadata {
		partsMetadata[index].Stat = xlMeta.Stat
		partsMetadata[index].Meta = xlMeta.Meta
		partsMetadata[index].Parts = xlMeta.Parts
	}
	// Write unique `xl.json` for each disk.
	if err = xl.writeUniqueXLMetadata(minioMetaBucket, tempUploadIDPath, partsMetadata); err != nil {
		return "", toObjectErr(err, minioMetaBucket, tempUploadIDPath)
	}
	rErr := xl.renameXLMetadata(minioMetaBucket, tempUploadIDPath, minioMetaBucket, uploadIDPath)
	if rErr != nil {
		return "", toObjectErr(rErr, minioMetaBucket, uploadIDPath)
	}
	// Hold write lock on the destination before rename
	nsMutex.Lock(bucket, object)
	defer nsMutex.Unlock(bucket, object)

	// Rename if an object already exists to temporary location.
	uniqueID := getUUID()
	err = xl.renameObject(bucket, object, minioMetaBucket, path.Join(tmpMetaPrefix, uniqueID))
	if err != nil {
		return "", toObjectErr(err, bucket, object)
	}

	// Remove parts that weren't present in CompleteMultipartUpload request
	for _, curpart := range currentXLMeta.Parts {
		if xlMeta.ObjectPartIndex(curpart.Number) == -1 {
			// Delete the missing part files. e.g,
			// Request 1: NewMultipart
			// Request 2: PutObjectPart 1
			// Request 3: PutObjectPart 2
			// Request 4: CompleteMultipartUpload --part 2
			// N.B. 1st part is not present. This part should be removed from the storage.
			xl.removeObjectPart(bucket, object, uploadID, curpart.Name)
		}
	}

	// Rename the multipart object to final location.
	if err = xl.renameObject(minioMetaBucket, uploadIDPath, bucket, object); err != nil {
		return "", toObjectErr(err, bucket, object)
	}

	// Delete the previously successfully renamed object.
	xl.deleteObject(minioMetaBucket, path.Join(tmpMetaPrefix, uniqueID))

	// Hold the lock so that two parallel complete-multipart-uploads do no
	// leave a stale uploads.json behind.
	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))

	// Validate if there are other incomplete upload-id's present for
	// the object, if yes do not attempt to delete 'uploads.json'.
	uploadsJSON, err := readUploadsJSON(bucket, object, xl.storageDisks...)
	if err == nil {
		uploadIDIdx := uploadsJSON.Index(uploadID)
		if uploadIDIdx != -1 {
			uploadsJSON.Uploads = append(uploadsJSON.Uploads[:uploadIDIdx], uploadsJSON.Uploads[uploadIDIdx+1:]...)
		}
		if len(uploadsJSON.Uploads) > 0 {
			if err = updateUploadsJSON(bucket, object, uploadsJSON, xl.storageDisks...); err != nil {
				return "", err
			}
			return s3MD5, nil
		}
	}

	err = xl.deleteObject(minioMetaBucket, path.Join(mpartMetaPrefix, bucket, object))
	if err != nil {
		return "", toObjectErr(err, minioMetaBucket, path.Join(mpartMetaPrefix, bucket, object))
	}

	// Return md5sum.
	return s3MD5, nil
}

// abortMultipartUpload - aborts a multipart upload.
func (xl xlObjects) abortMultipartUpload(bucket, object, uploadID string) error {
	// Verify if bucket is valid.
	if !IsValidBucketName(bucket) {
		return BucketNameInvalid{Bucket: bucket}
	}
	if !xl.isBucketExist(bucket) {
		return BucketNotFound{Bucket: bucket}
	}
	if !IsValidObjectName(object) {
		return ObjectNameInvalid{Bucket: bucket, Object: object}
	}

	// Hold lock so that there is no competing complete-multipart-upload or put-object-part.
	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object, uploadID))

	if !xl.isUploadIDExists(bucket, object, uploadID) {
		return InvalidUploadID{UploadID: uploadID}
	}

	// Cleanup all uploaded parts.
	if err := cleanupUploadedParts(bucket, object, uploadID, xl.storageDisks...); err != nil {
		return toObjectErr(err, bucket, object)
	}

	nsMutex.Lock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))
	defer nsMutex.Unlock(minioMetaBucket, pathJoin(mpartMetaPrefix, bucket, object))
	// Validate if there are other incomplete upload-id's present for
	// the object, if yes do not attempt to delete 'uploads.json'.
	uploadsJSON, err := readUploadsJSON(bucket, object, xl.storageDisks...)
	if err == nil {
		uploadIDIdx := uploadsJSON.Index(uploadID)
		if uploadIDIdx != -1 {
			uploadsJSON.Uploads = append(uploadsJSON.Uploads[:uploadIDIdx], uploadsJSON.Uploads[uploadIDIdx+1:]...)
		}
		if len(uploadsJSON.Uploads) > 0 {
			err = updateUploadsJSON(bucket, object, uploadsJSON, xl.storageDisks...)
			if err != nil {
				return toObjectErr(err, bucket, object)
			}
			return nil
		}
	}
	if err = xl.deleteObject(minioMetaBucket, path.Join(mpartMetaPrefix, bucket, object)); err != nil {
		return toObjectErr(err, minioMetaBucket, path.Join(mpartMetaPrefix, bucket, object))
	}
	return nil
}

// AbortMultipartUpload - aborts a multipart upload.
func (xl xlObjects) AbortMultipartUpload(bucket, object, uploadID string) error {
	return xl.abortMultipartUpload(bucket, object, uploadID)
}
