FROM node:alpine as build

ARG HANDLER

# Python and make are required by certain native package build processes in NPM packages.
ENV PYTHONUNBUFFERED=1
RUN apk add --update --no-cache python3 make g++ && ln -sf python3 /usr/bin/python
RUN python3 -m ensurepip
RUN pip3 install --no-cache --upgrade pip setuptools

RUN yarn global add typescript @vercel/ncc

COPY . .

RUN yarn import || echo Lockfile already exists

RUN set -ex; yarn install --production --frozen-lockfile --cache-folder /tmp/.cache; rm -rf /tmp/.cache;

RUN test -f tsconfig.json || echo "{\"compilerOptions\":{\"esModuleInterop\":true,\"target\":\"es2015\",\"moduleResolution\":\"node\"}}" > tsconfig.json

RUN ncc build ${HANDLER} -m --v8-cache -o lib/

FROM node:alpine as final

RUN apk update && \
    apk add --no-cache ca-certificates && \
    update-ca-certificates

COPY --from=build "package.json" "package.json"

COPY --from=build "node_modules/" "node_modules/"

COPY --from=build lib/ /

# Copy any other non-ignored assets to be included
COPY . .

ENTRYPOINT ["node", "index.js"]