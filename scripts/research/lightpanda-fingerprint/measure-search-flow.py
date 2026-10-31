"""Repeat the public search fixture; measure wall time and sampled process-tree RSS."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
from urllib.parse import urlparse


def process_tree_rss(root_pid):
    rows = subprocess.run(
        ['ps', '-axo', 'pid=,ppid=,rss='], capture_output=True, text=True, check=True
    ).stdout.splitlines()
    tree = {}
    for row in rows:
        fields = row.split()
        if len(fields) == 3:
            pid, parent, rss = map(int, fields)
            tree[pid] = (parent, rss)
    descendants = {root_pid}
    while True:
        added = {pid for pid, (parent, _) in tree.items() if parent in descendants}
        if added <= descendants:
            break
        descendants |= added
    return sum(tree[pid][1] for pid in descendants if pid in tree), len(descendants & tree.keys())


def measure(backend, command, env, round_index):
    started = time.monotonic()
    proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, start_new_session=True)
    peak_rss = 0
    peak_processes = 0
    samples = 0
    timed_out = False
    while proc.poll() is None:
        rss, count = process_tree_rss(proc.pid)
        peak_rss = max(peak_rss, rss)
        peak_processes = max(peak_processes, count)
        samples += 1
        if time.monotonic() - started > 60:
            timed_out = True
            os.killpg(proc.pid, signal.SIGTERM)
            break
        time.sleep(0.1)
    try:
        stdout, stderr = proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        stdout, stderr = proc.communicate()
    elapsed_ms = round((time.monotonic() - started) * 1000)
    # This group contains only this runner's command and its isolated browser.
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    result = None
    for line in stdout.splitlines():
        try:
            result = json.loads(line)
        except json.JSONDecodeError:
            continue
    return {'backend': backend, 'round': round_index, 'exitCode': proc.returncode,
            'timedOut': timed_out, 'wallMs': elapsed_ms, 'peakTreeRssKiB': peak_rss,
            'peakProcesses': peak_processes, 'rssSamples': samples,
            'result': result, 'stderr': stderr[-2000:]}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--lightpanda', required=True)
    parser.add_argument('--rounds', type=int, default=3, choices=range(1, 11))
    parser.add_argument('--out', required=True)
    parser.add_argument('--page-url')
    args = parser.parse_args()
    if args.page_url:
        parsed = urlparse(args.page_url)
        if parsed.scheme not in ['http', 'https'] or not parsed.hostname or parsed.username or parsed.password:
            parser.error('page-url must be a public HTTP(S) URL without userinfo')
    here = Path(__file__).resolve().parent
    env = dict(os.environ, LIGHTPANDA_DISABLE_TELEMETRY='true')
    rows = []
    version = subprocess.run([args.lightpanda, 'version'], capture_output=True,
                             text=True, check=True).stdout.strip()
    with tempfile.TemporaryDirectory(prefix='lightpanda-search-repeat-') as temp:
        lp_script = here / 'public-search-flow.js'
        if args.page_url:
            lp_script = Path(temp) / 'public-page.js'
            extraction = "({title:document.title,url:location.href,text:document.body.innerText.replace(/\\s+/g,' ').trim(),quoteTexts:Array.from(document.querySelectorAll('.quote .text')).map(q=>q.innerText.trim())})"
            lp_script.write_text('const page = new Page();\nawait page.goto(' + json.dumps(args.page_url)
                + ', {waitUntil:"networkidle",timeout:15000});\nreturn JSON.parse(await page.evaluate('
                + json.dumps(extraction) + '));\n')
        for index in range(1, args.rounds + 1):
            order = ['lightpanda', 'chrome'] if index % 2 else ['chrome', 'lightpanda']
            for backend in order:
                local_env = dict(env)
                if backend == 'lightpanda':
                    command = [args.lightpanda, 'run', str(lp_script),
                               '--log-level', 'error']
                else:
                    local_env['CHROME_PROBE_PORT'] = str(19431 + index)
                    local_env['CHROME_PROBE_PROFILE'] = str(Path(temp) / f'chrome-{index}')
                    if args.page_url:
                        local_env['CHROME_PROBE_URL'] = args.page_url
                    command = ['node', str(here / 'chrome-search-flow.mjs')]
                row = measure(backend, command, local_env, index)
                rows.append(row)
                print(json.dumps({k: row[k] for k in ['backend', 'round', 'exitCode',
                      'timedOut', 'wallMs', 'peakTreeRssKiB', 'peakProcesses']}), flush=True)
    output = {'fixture': args.page_url or 'https://quotes.toscrape.com/search.aspx',
              'lightpandaVersion': version, 'rounds': args.rounds,
              'resourceMetric': 'sampled sum of descendant RSS, KiB; shared pages may be double-counted',
              'sampleIntervalMs': 100, 'rows': rows}
    path = Path(args.out)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(output, ensure_ascii=False, indent=2) + '\n')


if __name__ == '__main__':
    main()
