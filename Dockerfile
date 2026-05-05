ARG BASE_IMAGE_TAG=base

FROM alpine:edge AS builder
LABEL stage=go-builder
WORKDIR /app/
RUN apk add --no-cache bash gcc git go musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY ./ ./

# 🔑 备份 chunk 前端（build.sh 会删掉它）
RUN cp -r public/dist /tmp/chunk-dist

# 造假 git
RUN git init && \
    git config user.email "build@docker" && \
    git config user.name "Docker Build" && \
    git add -A && \
    git commit -m "build"

# 🔑 让 build.sh 随便跑，覆盖就覆盖
RUN bash build.sh dev docker

# 🔑 关键：把 chunk 前端恢复回去，重新编译二进制
RUN rm -rf public/dist && \
    cp -r /tmp/chunk-dist public/dist && \
    builtAt="$(date +'%F %T %z')" && \
    gitCommit=$(git log --pretty=format:"%h" -1) && \
    ldflags="\
-w -s \
-X 'github.com/OpenListTeam/OpenList/v4/internal/conf.BuiltAt=$builtAt' \
-X 'github.com/OpenListTeam/OpenList/v4/internal/conf.GitCommit=$gitCommit' \
-X 'github.com/OpenListTeam/OpenList/v4/internal/conf.Version=dev-chunk' \
-X 'github.com/OpenListTeam/OpenList/v4/internal/conf.WebVersion=chunk-rolling' \
" && \
    CGO_ENABLED=1 go build -o bin/openlist -tags=jsoniter -ldflags="$ldflags" .

FROM openlistteam/openlist-base-image:${BASE_IMAGE_TAG}
LABEL MAINTAINER="OpenList"
ARG INSTALL_FFMPEG=false
ARG INSTALL_ARIA2=false
ARG USER=openlist
ARG UID=1001
ARG GID=1001

WORKDIR /opt/openlist/

RUN addgroup -g ${GID} ${USER} && \
    adduser -D -u ${UID} -G ${USER} ${USER} && \
    mkdir -p /opt/openlist/data

COPY --from=builder --chmod=755 --chown=${UID}:${GID} /app/bin/openlist ./
COPY --chmod=755 --chown=${UID}:${GID} entrypoint.sh /entrypoint.sh

USER ${USER}
RUN /entrypoint.sh version

ENV UMASK=022 RUN_ARIA2=${INSTALL_ARIA2}
VOLUME /opt/openlist/data/
EXPOSE 5244 5245
CMD [ "/entrypoint.sh" ]
