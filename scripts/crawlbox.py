#!/usr/bin/env python3
"""Crawlbox 公开 API CLI，Python 3.9+，仅使用标准库。

填写下方 BASE_URL、TOKEN 后运行：
  python3 crawlbox.py sources
  python3 crawlbox.py revisions source-a
  python3 crawlbox.py files source-a
  python3 crawlbox.py file source-a data.db -o data.db
  python3 crawlbox.py artifacts source-b
  python3 crawlbox.py artifact source-b index.db -o index.db
  python3 crawlbox.py archive source-b -r REVISION_ID -o archive.tar.gz

列表和信息输出 JSON，可用 > 保存；revisions/files/artifacts 自动翻页。
-r 默认 latest，支持 revision ID（不是上游版本 tag）。下载覆盖指定输出文件。
"""

import argparse
import gzip
import hashlib
import http.client
import json
import os
from pathlib import Path
import sys
import ssl
import tempfile
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode
from urllib.request import Request, urlopen

BASE_URL = "http://127.0.0.1:8080"  # 填写 NAS manager 地址，也接受以 /api/v1 结尾
TOKEN = ""  # 填写 Web UI 创建的 API token
VERIFY_SSL = True  # 自签名、过期等证书设为 False：跳过证书链和主机名校验
TIMEOUT = 1800  # 秒；首次历史恢复/归档可能较慢，属于 socket 等待超时
CHUNK = 1024 * 1024


class Client:
    def __init__(self, base_url, token, timeout, verify_ssl=None):
        self.base = base_url.rstrip("/")
        if not self.base.endswith("/api/v1"):
            self.base += "/api/v1"
        self.token, self.timeout = token, timeout
        if verify_ssl is None:
            verify_ssl = VERIFY_SSL
        self.ssl_context = ssl.create_default_context()
        if not verify_ssl:
            self.ssl_context.check_hostname = False
            self.ssl_context.verify_mode = ssl.CERT_NONE

    def open(self, path, byte_range=None):
        headers = {"Authorization": "Bearer " + self.token}
        if byte_range:
            headers["Range"] = "bytes=" + byte_range
        return urlopen(Request(self.base + path, headers=headers), timeout=self.timeout, context=self.ssl_context)

    def json(self, path):
        with self.open(path) as response:
            return json.load(response)

    def entries(self, path):
        entries, offset = [], 0
        while True:
            page = self.json(path + "?" + urlencode({"offset": offset, "limit": 1000}))
            entries.extend(page["entries"] or [])
            if page["next_offset"] >= page["total"]:
                return entries
            if page["next_offset"] <= offset:
                raise ValueError("服务器返回的分页游标没有前进")
            offset = page["next_offset"]

    def download(self, path, output, byte_range=None, archive=False):
        output = Path(output)
        output.parent.mkdir(parents=True, exist_ok=True)
        temporary = None
        try:
            with self.open(path, byte_range) as response:
                if byte_range and response.status != 206:
                    raise ValueError("服务器未返回部分内容（206），未保存文件")
                expected = response.headers.get("Content-Length")
                etag = response.headers.get("ETag", "").strip('"')
                digest, size = hashlib.sha256(), 0
                with tempfile.NamedTemporaryFile(dir=output.parent, delete=False) as stream:
                    temporary = Path(stream.name)
                    while True:
                        chunk = response.read(CHUNK)
                        if not chunk:
                            break
                        stream.write(chunk)
                        digest.update(chunk)
                        size += len(chunk)
                if expected is not None and size != int(expected):
                    raise ValueError("下载不完整：文件大小与 Content-Length 不符")
                if not byte_range and len(etag) == 64 and digest.hexdigest() != etag:
                    raise ValueError("下载文件的 SHA-256 与服务器 ETag 不符")
            if archive:
                # 未缓存归档可能没有 Content-Length；读取 gzip 尾部以识别中途截断。
                with gzip.open(temporary, "rb") as stream:
                    while stream.read(CHUNK):
                        pass
            os.replace(temporary, output)
            print(f"已保存 {output}（{size:,} 字节）", file=sys.stderr)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)


def parser():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--base-url", default=BASE_URL)
    p.add_argument("--token", default=TOKEN)
    p.add_argument("--timeout", type=float, default=TIMEOUT)
    commands = p.add_subparsers(dest="command", required=True)
    commands.add_parser("sources", help="列出有权限且已发布的数据源")
    for name in ("revisions", "tags", "info", "changes", "files", "artifacts", "file", "artifact", "archive"):
        sub = commands.add_parser(name)
        sub.add_argument("source", help="source ID")
        if name not in ("revisions", "tags"):
            sub.add_argument("-r", "--revision", default="latest", help="revision ID 或 latest（默认）")
        if name in ("file", "artifact"):
            sub.add_argument("path", help="列表中的完整逻辑路径")
            sub.add_argument("--range", dest="byte_range", help="可选字节范围，例如 0-1023")
        if name in ("file", "artifact", "archive"):
            sub.add_argument("-o", "--output", required=True, help="本地保存路径")
    return p


def main(argv=None):
    p = parser()
    args = p.parse_args(argv)
    if not args.token.strip():
        p.error("请在脚本顶部填写 TOKEN，或通过 --token 指定")
    if args.timeout <= 0:
        p.error("--timeout 必须大于 0")
    client = Client(args.base_url, args.token.strip(), args.timeout)
    route = "/sources"
    if args.command != "sources":
        route += "/" + quote(args.source, safe="")
        if args.command in ("revisions", "tags"):
            route += "/" + args.command
        else:
            route += "/revisions/" + quote(args.revision, safe="")
            if args.command in ("files", "artifacts"):
                # 固定 revision，避免翻页时 latest 更新导致列表混合两个版本。
                revision = client.json(route + "/metadata")["id"]
                route = "/sources/" + quote(args.source, safe="") + "/revisions/" + quote(revision, safe="")
            kind = {"info": "metadata", "file": "files", "artifact": "artifacts"}.get(args.command, args.command)
            route += "/" + kind
            if args.command in ("file", "artifact"):
                route += "/" + quote(args.path, safe="/")
    if args.command in ("file", "artifact", "archive"):
        client.download(route, args.output, getattr(args, "byte_range", None), args.command == "archive")
    else:
        paginated = ("revisions", "files", "artifacts")
        result = client.entries(route) if args.command in paginated else client.json(route)
        print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except HTTPError as error:
        hints = {401: "token 缺失、无效、过期或已撤销", 404: "资源不存在或 token 无权访问", 416: "字节范围不可满足"}
        print(f"HTTP {error.code}: {hints.get(error.code, error.reason)}", file=sys.stderr)
        sys.exit(1)
    except (OSError, URLError, ValueError, EOFError, http.client.HTTPException) as error:
        print(f"失败：{error}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("已取消", file=sys.stderr)
        sys.exit(130)
