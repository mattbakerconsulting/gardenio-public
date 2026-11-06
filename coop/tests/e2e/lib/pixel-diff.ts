// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

import Pixelmatch from "pixelmatch";
import { PNG } from "pngjs";

export interface DiffResult {
	diffPercent: number;
	diffBuffer: Buffer;
}

/** Compare two PNG screenshot buffers and return a diff percentage + diff image. */
export function compareScreenshots(
	bufA: Buffer,
	bufB: Buffer,
): DiffResult {
	const imgA = PNG.sync.read(bufA);
	const imgB = PNG.sync.read(bufB);

	// Use the larger dimensions to ensure both fit
	const width = Math.max(imgA.width, imgB.width);
	const height = Math.max(imgA.height, imgB.height);

	// Pad images to the same dimensions if needed
	const padded = (img: PNG): Buffer => {
		if (img.width === width && img.height === height) return img.data as Buffer;
		const buf = Buffer.alloc(width * height * 4, 0);
		for (let y = 0; y < img.height; y++) {
			const srcOff = y * img.width * 4;
			const dstOff = y * width * 4;
			(img.data as Buffer).copy(buf, dstOff, srcOff, srcOff + img.width * 4);
		}
		return buf;
	};

	const dataA = padded(imgA);
	const dataB = padded(imgB);

	const diff = new PNG({ width, height });
	const mismatch = Pixelmatch(dataA, dataB, diff.data as Buffer, width, height, {
		threshold: 0.1,
	});

	const totalPixels = width * height;
	const diffPercent = (mismatch / totalPixels) * 100;

	return {
		diffPercent,
		diffBuffer: PNG.sync.write(diff),
	};
}
