# earbug

Log spotify listening history

[![License](https://img.shields.io/github/license/seankhliao/earbug.svg?style=for-the-badge&maxAge=31536000)](LICENSE)
[![Build](https://badger.seankhliao.com/i/github_seankhliao_earbug)](https://badger.seankhliao.com/l/github_seankhliao_earbug)

## About

Wanted log of what I listen to, for future work

runs every 15 min (30s min x 50 songs max - retry buffer), logs to GCP storage bucket with dedupe

## Usage

#### Prerequisites

- GCP storage
  - default uses service account credentials
- Spotify dev token

env:

- `BUCKET`: gcp storage bucket
- `SPOTIFY_ID`: spotify id from dev console
- `SPOTIFY_SECRET`: spotify secret from dev console

#### Install

go:

```sh
go get github.com/seankhliao/earbug
```

#### Run

```sh
# generate token file, interactive, requires browser and port 8910 on localhost
earbug auth
  -t token.json, path to save token to


earbug [-t token.json] [-i 15m]
  -t spotify access token file
  -i interval between updates
```

docker:

```sh
docker run --rm \
  -v /path/to/spotify-token.json:/app/token.json \
  -e BUCKET=gcp_bucket_name \
  -e SPOTIFY_ID=spotify_id \
  -e SPOTIFY_SECRET=spotify_secret \
  seankhliao/earbug

```

#### Build

docker:

```sh
docker build \
  --network host \
  .
```

## Todo

- [ ] save only trackID, start time and duration
- [ ] use single file per month / year
