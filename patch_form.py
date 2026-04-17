import os

path = r"f:\Github_program\openlist-chunk\OpenList-Frontend-main\src\pages\home\uploads\form.ts"
with open(path, "r", encoding="utf-8") as f:
    orig = f.read()

# 1. Replace generateUploadId with fetchUploadId
old_generate = """// Generate a unique upload ID based on path, size, and file hash
async function generateUploadId(path: string, file: File): Promise<string> {
  const sample = file.slice(0, Math.min(1024 * 1024, file.size))
  const buffer = await sample.arrayBuffer()
  const hashBuffer = await crypto.subtle.digest("SHA-256", buffer)
  const hashHex = Array.from(new Uint8Array(hashBuffer))
    .slice(0, 8)
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("")
  // Use encodeURIComponent to handle Unicode characters before btoa
  const rawId = `${path}|${file.size}|${hashHex}`
  const encodedId = btoa(encodeURIComponent(rawId))
  return encodedId.replace(/[+/=]/g, "_")
}"""

new_fetch = """// Fetch a secure upload ID from the server
async function fetchUploadId(path: string, file: File, totalChunks: number): Promise<string> {
  const resp: any = await r.post("/fs/put/chunk/init", {
    path: path,
    total_chunks: totalChunks,
    last_modified: file.lastModified,
  }, {
    headers: {
      password: password()
    }
  })
  
  if (resp.code !== 200) {
    throw new Error(`Failed to initialize chunked upload: ${resp.message}`)
  }
  return resp.data.upload_id
}"""

if old_generate in orig:
    orig = orig.replace(old_generate, new_fetch)
else:
    print("Cannot find old_generate")

# 2. Update chunkedUpload to use fetchUploadId
old_chunk_start_1 = """  // Generate upload ID
  const uploadId = await generateUploadId(uploadPath, file)

  // Split file into chunks
  const chunks = splitFile(file, chunkSize)
  const totalChunks = chunks.length"""

new_chunk_start_1 = """  // Split file into chunks
  const chunks = splitFile(file, chunkSize)
  const totalChunks = chunks.length

  // Generate upload ID securely from server
  const uploadId = await fetchUploadId(uploadPath, file, totalChunks)"""

if old_chunk_start_1 in orig:
    orig = orig.replace(old_chunk_start_1, new_chunk_start_1)
else:
    print("Cannot find old_chunk_start_1")

with open(path, "w", encoding="utf-8") as f:
    f.write(orig)
print("SUCCESS form patch")
