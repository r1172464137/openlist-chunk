import json
import os

langs_dir = r"f:\code_program\OpenList-4.1.9\OpenList-Frontend-main\src\lang"
en_path = os.path.join(langs_dir, "en", "settings.json")
cn_path = os.path.join(langs_dir, "zh-CN", "settings.json")

def patch_file(path, updates):
    if not os.path.exists(path):
        print(f"File not found: {path}")
        return
        
    with open(path, 'r', encoding='utf-8') as f:
        data = json.load(f)
        
    for k, v in updates.items():
        data[k] = v
        
    with open(path, 'w', encoding='utf-8') as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
    print(f"Patched {path}")

en_updates = {
    "chunked_upload_mode": "Chunked Upload Mode",
    "chunked_upload_mode-tips": "Controls chunked upload behaviour. 'auto' (default): chunked upload is used automatically when the file exceeds the threshold, allowing uploads to bypass CDN size limits. 'disabled': always use direct upload regardless of file size, suitable for environments without CDN restrictions.",
    "chunked_upload_chunk_size": "Chunked Upload Threshold (MB)",
    "chunked_upload_chunk_size-tips": "File size threshold in MB for triggering chunked upload. Only effective in 'auto' mode. Set to 1 to chunk almost all files. Default: 95 (just below Cloudflare's 100MB free-tier limit)."
}

cn_updates = {
    "分片上传模式": "chunked_upload_mode",
    "chunked_upload_mode": "分片上传模式",
    "chunked_upload_mode-tips": "控制分片上传的开关。auto（默认）：超过阈值的文件自动分片，绕过 CDN 大小限制。disabled：始终使用直接上传，适用于无 CDN 限制的内网内网环境。",
    "chunked_upload_chunk_size": "分片上传阈值（MB）",
    "chunked_upload_chunk_size-tips": "触发分片上传的文件大小阈值（MB），仅在 auto 模式下生效。设为 1 可让几乎所有文件都走分片。默认 95 （绕过 Cloudflare 免费版 100MB 限制）。"
}

patch_file(en_path, en_updates)
patch_file(cn_path, cn_updates)
