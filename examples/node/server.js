const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const directory = process.env.DATA_DIR || '/data';
fs.mkdirSync(directory, { recursive: true });
const counterFile = path.join(directory, 'visits.txt');
let visits = fs.existsSync(counterFile)
  ? Number(fs.readFileSync(counterFile, 'utf8')) || 0
  : 0;

const server = http.createServer((request, response) => {
  if (request.url === '/healthz') {
    response.writeHead(200, { 'Content-Type': 'text/plain' });
    response.end('ok\n');
    return;
  }

  if (request.url === '/stream') {
    response.writeHead(200, {
      'Content-Type': 'text/plain',
      'Cache-Control': 'no-cache',
    });
    response.write('This request keeps the application running for 30 seconds.\n');
    const interval = setInterval(() => response.write(`${new Date().toISOString()}\n`), 1000);
    const timeout = setTimeout(() => response.end('Stream finished.\n'), 30000);
    response.on('close', () => {
      clearInterval(interval);
      clearTimeout(timeout);
    });
    return;
  }

  if (request.url !== '/') {
    response.writeHead(404, { 'Content-Type': 'text/plain' });
    response.end('Not found\n');
    return;
  }

  visits += 1;
  fs.writeFileSync(counterFile, String(visits));
  response.writeHead(200, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify({
    message: 'Hello from a ZIP deployment',
    visits,
    persistence: 'The counter lives in a named Docker volume.',
  }) + '\n');
});

server.listen(Number(process.env.PORT || 8080), '0.0.0.0', () => {
  console.log(`Listening on port ${process.env.PORT || 8080}`);
});
process.on('SIGTERM', () => server.close());
