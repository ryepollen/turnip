// No fetch handler: the page keeps loading from the network.
// The worker only exists so /wegweiser/sw.js is a real file.
self.addEventListener('install', function () { self.skipWaiting(); });
self.addEventListener('activate', function (event) {
  event.waitUntil(self.clients.claim());
});
