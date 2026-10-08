// Sizes the print page to the .frame element so the exported PDF/SVG has no
// page margins. Runs before Chromium's --print-to-pdf snapshot.
(() => {
  const frame = document.querySelector(".frame");
  const { width, height } = frame.getBoundingClientRect();
  const style = document.createElement("style");
  style.textContent = `@page { size: ${Math.ceil(width)}px ${Math.ceil(height)}px; margin: 0; }`;
  document.head.appendChild(style);
})();
