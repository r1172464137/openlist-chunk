# OpenList 分片上传架构级重构与迁移详细指南

由于原生单文件直传方案易受 CDN 大小限制（如 Cloudflare 100MB 拦截），本项目对其上传链路进行了深度重构。包含了从前端的分块调度机制、按需开关选项，到后端的 Token 会话身份防篡改、硬盘 `io.MultiReader` 虚拟流合并，以及核心 `skipHook` 执行错误的彻底修复。

如果在后续升级官方大版本（如 v4.2+）时需要保留分片功能，**请严格核对并迁移以下所有代码片段**。

---

## 一、 前端核心链路修改 (Frontend)

### 1. 替换调度器逻辑与增加动态开关
**文件位置**: `OpenList-Frontend-main/src/pages/home/uploads/form.ts`

**片段 A：新增安全获取 `upload_id` 方法**
（替换原来的纯前端 Hash 生成，改为从后端初始化分片会话）：
```typescript
// Fetch a secure upload ID from the server
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
}
```

**片段 B：新增开关状态读取方法**
（插在文件末尾 `export const FormUpload` 之前）：
```typescript
// Read mode from server settings
const getChunkMode = (): "auto" | "disabled" => {
  const mode = getSetting("chunked_upload_mode")
  if (mode === "disabled") return "disabled"
  return "auto" // Default for any unknown / missing value
}
```

**片段 C：修改主干控制流逻辑 (`FormUpload` 函数内)**
```typescript
export const FormUpload: Upload = async (
  uploadPath: string,
  file: File,
  // ... 其他参数 ...
) => {
  const mode = getChunkMode()
  const chunkSize = getChunkSize()
  const fileSizeMB = (file.size / 1024 / 1024).toFixed(2)
  const chunkSizeMB = (chunkSize / 1024 / 1024).toFixed(0)

  // Use chunked upload for large files if auto mode is enabled (替换原来的 if)
  if (mode === "auto" && file.size > chunkSize) {
    console.log(`[Form Upload] ${file.name} > ${chunkSizeMB} MB threshold (Mode: ${mode}), using chunked upload`)
    return chunkedUpload(...)
  }

  // Use direct upload otherwise
  console.log(`[Form Upload] ${file.name} using direct upload (Mode: ${mode})`)
  return directUpload(uploadPath, file, setUpload, asTask, overwrite, rapid)
}
```

*(注意：由于删除了老版本依赖于第三方 `xxhash` 的 `calculateXXHash64`，如果项目存在老代码，请将 `util.ts` 中的 `calculateHash` 还原为原生的 MD5/SHA 哈希逻辑以支持“秒传”)*

### 2. 多语言字典注入
**文件位置**: `src/lang/en/settings.json` 和 `src/lang/zh-CN/settings.json`
加入新的展示词条：
```json
// zh-CN
"chunked_upload_mode": "分片上传模式",
"chunked_upload_mode-tips": "建议设为 auto。当设为 disabled 时彻底关闭分片走直传。",
"chunked_upload_chunk_size": "分片大小阈值(MB)",
"chunked_upload_chunk_size-tips": "超过此大小即切片。默认95MB（以绕过 CF 限制）。"

// en 对应翻译同理
```

---

## 二、 后端核心链路修改 (Backend)

### 1. 挂载路由 API
**文件位置**: `server/router.go`
在 `_fs(g *gin.RouterGroup)` 或者 `fsAndShare` 中注入分片的 3 个 API 接口：
```go
	g.PUT("/form", middlewares.FsUp, uploadLimiter, handles.FsForm)
	// 【新增分片上传的三个核心路由】
	g.POST("/put/chunk/init", middlewares.FsUp, handles.FsChunkInit)
	g.PUT("/put/chunk", middlewares.FsUp, uploadLimiter, handles.FsChunkUpload)
	g.POST("/put/chunk/merge", middlewares.FsUp, uploadLimiter, handles.FsChunkMerge)
```

### 2. 常量与数据库配置表注册
**文件位置 A**: `internal/conf/const.go`
```go
	ChunkedUploadMode      = "chunked_upload_mode"       // 分片上传开关: auto(默认)/disabled
	ChunkedUploadChunkSize = "chunked_upload_chunk_size" // 分片阈值(MB)
```

**文件位置 B**: `internal/bootstrap/data/setting.go` （在 `InitialSettings` 函数返回数组中加入）
```go
		{
			Key:     conf.ChunkedUploadMode,
			Value:   "auto",
			Type:    conf.TypeSelect,
			Options: "auto,disabled",
			Group:   model.TRAFFIC,
			Flag:    model.PUBLIC,
			Help:    "Chunked upload mode. 'auto': use chunked upload when file exceeds threshold (recommended). 'disabled': always use direct upload, no chunking.",
		},
		{
			Key:   conf.ChunkedUploadChunkSize,
			Value: "95",
			Type:  conf.TypeNumber,
			Group: model.TRAFFIC,
			Flag:  model.PUBLIC,
			Help:  "Chunked upload size threshold (MB). Only effective in 'auto' mode. Default: 95.",
		},
```

### 3. 最核心：上传逻辑安全与性能重构
**文件位置**: `server/handles/fsup.go`

这部分重构代码极大，主要包含 5 个核心技术点：修 Bug（文件句柄被意外提前关闭、存储钩子无法掉起的问题）以及防黑客越权合并的会话锁定机制。

**片段 A：修复原生上传函数中导致原生网盘 hook 失效的 Bug**
将下面几个函数里的 `fs.PutDirectly(ctx, dir, s, true)` 最后一个参数统统改为 `false` 或者去除以支持后置动作：
- `fsStreamChunked` 
- `fsStreamDirect` 
- `FsForm`
```go
	// 正确的写法（原先错误值为 true，导致不走扩展程序 hook）
	err := fs.PutDirectly(context.Background(), dir, s, false) 
	// 或
	err = fs.PutDirectly(c.Request.Context(), dir, s) 
```

**片段 B：提取核心鉴权与校验工具函数**
在 `fsup.go` 顶部统一封装上下文提取和权限拦截逻辑，消除大量的冗余卫语句：
```go
// 统一的上下文参数提取与绝对路径解析
func getUserFromContext(c *gin.Context) *model.User { return ... }
func resolveUserPath(c *gin.Context, rawPath string) (string, error) { return ... }

// 上传权限、防覆盖校验以及过滤系统文件
func checkWritePermission(path string) error { ... }
func checkFileExists(ctx context.Context, path string, overwrite bool) error { ... }
func validateFileName(name string) error { ... }

// 统一的分片临时路径计算与会话归属权鉴定
func getChunkDir(uploadId string) string { return ... }
func getAndVerifyChunkSession(c *gin.Context, uploadId string) (map[string]interface{}, string, error) {
	chunkDir := getChunkDir(uploadId)
	// ... 提取 session.json，核对 sessionData["user_id"] == getUserFromContext(c).ID
	return sessionData, chunkDir, nil
}
```

**片段 C：修改 `FsChunkInit` 利用抽象逻辑签发会话**
通过 UUID 颁发任务号，同时利用抽象好的校验函数，实现整洁安全的初始化拦截：
```go
// FsChunkInit securely allocates an upload_id and stores session info
func FsChunkInit(c *gin.Context) {
	// ... 解析传参 req ...
	
	// 1. 提权路径并拦截非法无权目标写入
	path, err := resolveUserPath(c, req.Path)
	if err != nil { return }
	if err := checkWritePermission(path); err != nil { return }

	// 2. 分配分片专用目录
	uploadId := uuid.NewString()
	chunkDir := getChunkDir(uploadId)
	
	// 3. 写入 {"user_id": getUserFromContext(c).ID, ...} 等验证状态持久化并返回给前端
	// ...
}
```

**片段 D：修改分片上传 API (`FsChunkUpload` 及 `FsChunkMerge`)**
在真实落盘或合并流前，调用高级校验函数，一并获取权限与临时工作目录：
```go
func FsChunkUpload(c *gin.Context) {
	uploadId := c.Query("upload_id")
	indexStr := c.Query("index")
	
	// 高度抽象：同时获取会话信息与工作目录，一举两得
	_, chunkDir, err := getAndVerifyChunkSession(c, uploadId)
	if err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}
	// chunkPath := stdpath.Join(chunkDir, indexStr) ...
	// 继续执行后续的分片磁盘暂存 ...
}
```

**片段 E：引入 `io.MultiReader` 消除合并开销**
实现一个无需将切片物理拼接在一起、能够在读取时直接将碎片当成一个整体投递给存储端的方法：
```go
// buildMergeStream prepares an io.MultiReader to stream all chunks without copying
func buildMergeStream(chunkDir, name string, totalChunks int, lastModified time.Time, expectedHash string) (*stream.FileStream, error) {
	var readers []io.Reader
	var closers []io.Closer
    // ... 将 0 - totalChunks 每一个小文件以 os.Open 打破装入 readers ...
    
	multiReader := io.MultiReader(readers...)
    
	s := &stream.FileStream{
		Obj: &model.Object{Name: name, Size: totalSize, Modified: lastModified},
		Reader:   multiReader, // 极其关键：将流接口交给框架，它边读边汇聚
		Mimetype: utils.GetMimeType(name),
	}
    // 加入自动清理 defer 操作
	s.Closers.Add(utils.CloseFunc(func() error {
		for _, c := range closers {
			c.Close()
		}
		os.RemoveAll(chunkDir)
		return nil
	}))

	return s, nil
}
```

**片段 F：更新 `FsChunkMerge` 实现无感传输**
移除冗长的物理合并拷贝逻辑（去掉了老版的异步后台 `go func() copy()`）：
```go
func FsChunkMerge(c *gin.Context) {
	// ... 解析传参 req ...
	sessionData, chunkDir, err := getAndVerifyChunkSession(c, req.UploadId)
	if err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}

	reqPath := sessionData["path"].(string)
	
	// ... 构建流 ...
	s, err := buildMergeStream(chunkDir, name, totalChunks, lastModified, req.Hash)
	
	s.WebPutAsTask = req.AsTask
	var t task.TaskExtensionInfo
	if req.AsTask {
		t, err = fs.PutAsTask(c.Request.Context(), dir, s)
	} else {
		err = fs.PutDirectly(c.Request.Context(), dir, s) // <= 由于前面改了，这里会正确触发后端 Hook
	}
    // ... 清理资源 ...
}
```

---

## 三、 特殊构建注意事项 (Build Protocol)

### 1. 静态环境嵌入（Embed）机制
由于 OpenList 项目采用的是 `//go:embed public/dist`，这意味着前端界面会被打包成单体应用 (`.exe` 或 ELF)。如果在编译步骤弄反了顺序，程序依旧会运行“旧版本”网页。

**编译流程规定**：
1. **完成前端 `npm build`**。
2. 将得到的静态资源（`dist/` 里的 `index.html`, `/assets` 等）同步拷贝并覆盖到后端仓库树的 `f:\...\public\dist\` 中。
3. 如果目标部署环境是没有 C 语言编译支持（比如缺少 Glibc 和 MSYS 链接库的裸 Linux/Windows），则必须开启静态链接将 SQLite CGO 完全打入执行文件。

**Windows 标准带 CGO 静态链接的构建命令：**
```powershell
# 必须先执行完拷贝覆盖
$env:CGO_ENABLED="1"
$env:CC="gcc"
go build -o openlist_latest.exe -tags=jsoniter -ldflags="-s -w -extldflags '-static'" .
```

### 2. 浏览器缓存穿透
因前端 SPA 单页面应用的缓存在绝大多数用户侧都是极强的（尤其是 ServiceWorker 环境下），升级服务端 Binary 后，**必须要告知测试/运维人员在浏览器页面内通过 `Ctrl + Shift + R` 进行强制刷新**！

以此消除浏览器内保留的带过时切片 API 请求路由的“僵尸JS堆”，防止访问到 `/api/fs/out/chunk` （旧命名）导致的报错。
