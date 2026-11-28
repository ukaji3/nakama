// Submits the form that signs the browser in to code-server or Kiro
// Crew once the streamed page is complete (nakama control panel).
'use strict';
document.addEventListener('DOMContentLoaded', function () {
  var f = document.getElementById('nakama-auto');
  if (f) { f.submit(); }
});
