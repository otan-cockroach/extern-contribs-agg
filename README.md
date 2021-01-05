# extern-contribs-agg

The code here aggregates external contributions from organizations.

## Usage

* Clone the repo: `git clone github.com/otan-cockroach/extern-contribs-agg`.
* Grab a [Personal Access Token](https://docs.github.com/en/free-pro-team@latest/github/authenticating-to-github/creating-a-personal-access-token) and set it in your environment: `export GITHUB_API_KEY="<key>"`.
* Run the program:
  * For all output, run `go run .`
  * For specific dates, run `go run . --start_date=2020-12-01 --end_date=2020-12-31`.
* The output is saved is markdown in `output.md`, but also printed on screen. You can display this on an online markdown viewer, such as [https://markdownlivepreview.com/](https://markdownlivepreview.com/).
