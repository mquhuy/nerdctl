# Test Plan for --skip-existing-layers Flag

## Overview
This test plan validates the implementation of the `--skip-existing-layers` flag for the `nerdctl push` command.

## Implementation Summary

### Changes Made:

1. **Added new flag to CLI**: `--skip-existing-layers`
   - File: `cmd/nerdctl/image/image_push.go`
   - Flag description: "Skip checking if layers already exist in registry (push all layers)"

2. **Extended ImagePushOptions type**: Added `SkipExistingLayers bool` field
   - File: `pkg/api/types/image_types.go`

3. **Updated push function signature**: Added `skipExistingLayers bool` parameter
   - File: `pkg/imgutil/push/push.go`

4. **Created custom HTTP client**: Intercepts HEAD requests to force layer uploads
   - File: `pkg/imgutil/push/nocheckresolver.go`
   - Creates `noCheckHTTPClient` that returns 404 for HEAD requests to `/blobs/` endpoints

5. **Modified push logic**: Uses custom resolver when flag is set
   - File: `pkg/cmd/image/push.go`
   - Calls `push.NewNoCheckResolver()` when `options.SkipExistingLayers` is true

## Test Cases

### 1. Basic Functionality Test
```bash
# Build an image locally
nerdctl build -t test-image .

# Push with the new flag
nerdctl push --skip-existing-layers test-image:latest

# Expected: All layers are pushed without HEAD requests
```

### 2. Compare with Default Behavior
```bash
# Push without flag (normal behavior)
nerdctl push test-image:latest

# Push with flag
nerdctl push --skip-existing-layers test-image:latest

# Expected: Second push should take longer as it uploads all layers again
```

### 3. Network Monitoring Test
```bash
# Monitor network requests during push
tcpdump -i any -s 0 -w push_test.pcap &
nerdctl push --skip-existing-layers test-image:latest
killall tcpdump

# Expected: No HEAD requests to /v2/*/blobs/* endpoints in capture
```

### 4. Help Text Test
```bash
nerdctl push --help | grep skip-existing-layers
# Expected: Shows flag description
```

## Implementation Details

### How it Works:

1. **Custom HTTP Client**: The `noCheckHTTPClient` wraps the standard HTTP client
2. **HEAD Request Interception**: When a HEAD request is made to a `/blobs/` endpoint, it returns a 404 response
3. **Registry Behavior**: Registry clients interpret 404 as "blob doesn't exist" and proceed with upload
4. **Bypass Strategy**: This effectively bypasses the layer existence check without modifying containerd's core logic

### Key Files Modified:

- `pkg/imgutil/push/nocheckresolver.go`: Custom resolver implementation
- `pkg/cmd/image/push.go`: Integration with main push logic
- `cmd/nerdctl/image/image_push.go`: CLI flag definition
- `pkg/api/types/image_types.go`: Options struct extension

## Expected Behavior

### When flag is NOT set (default):
- Registry HEAD requests check if layers exist
- Only missing layers are uploaded
- Faster push for layers that already exist

### When flag IS set:
- HEAD requests to `/blobs/` return 404 (simulated)
- Registry client uploads all layers
- Slower push but ensures all layers are uploaded

## Validation Steps

1. ✅ Code compiles successfully
2. ✅ CLI flag is properly defined
3. ✅ Options are passed through the call chain
4. ✅ Custom resolver is used when flag is set
5. ⏳ Integration testing with real registry
6. ⏳ Network capture analysis

## Notes

- The implementation preserves existing behavior when flag is not used
- No changes to containerd's core registry client
- Uses HTTP client interception for clean separation of concerns
- Should work with all registry types (Docker Hub, ECR, etc.)