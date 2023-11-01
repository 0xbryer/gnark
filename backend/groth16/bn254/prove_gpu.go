//go:build icicle
// +build icicle

// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package groth16

import (
	"math/big"
	"time"
	"unsafe"

	"github.com/consensys/gnark-crypto/ecc"
	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/pedersen"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/groth16/internal"
	"github.com/consensys/gnark/backend/witness"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/constraint/solver"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gnark/logger"
	iciclegnark "github.com/ingonyama-zk/iciclegnark/curves/bn254"
)

// Proof represents a Groth16 proof that was encoded with a ProvingKey and can be verified
// with a valid statement and a VerifyingKey
// Notation follows Figure 4. in DIZK paper https://eprint.iacr.org/2018/691.pdf
type Proof struct {
	Ar, Krs       curve.G1Affine
	Bs            curve.G2Affine
	Commitments   []curve.G1Affine // Pedersen commitments a la https://eprint.iacr.org/2022/1072
	CommitmentPok curve.G1Affine   // Batched proof of knowledge of the above commitments
}

// isValid ensures proof elements are in the correct subgroup
func (proof *Proof) isValid() bool {
	return proof.Ar.IsInSubGroup() && proof.Krs.IsInSubGroup() && proof.Bs.IsInSubGroup()
}

// CurveID returns the curveID
func (proof *Proof) CurveID() ecc.ID {
	return curve.ID
}

// Prove generates the proof of knowledge of a r1cs with full witness (secret + public part).
func Prove(r1cs *cs.R1CS, pk *ProvingKey, fullWitness witness.Witness, opts ...backend.ProverOption) (*Proof, error) {
	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return nil, err
	}

	log := logger.Logger().With().Str("curve", r1cs.CurveID().String()).Str("hardware", "GPU").Int("nbConstraints", r1cs.GetNbConstraints()).Str("backend", "groth16").Logger()

	commitmentInfo := r1cs.CommitmentInfo.(constraint.Groth16Commitments)

	proof := &Proof{Commitments: make([]curve.G1Affine, len(commitmentInfo))}

	solverOpts := opt.SolverOpts[:len(opt.SolverOpts):len(opt.SolverOpts)]

	privateCommittedValues := make([][]fr.Element, len(commitmentInfo))
	for i := range commitmentInfo {
		solverOpts = append(solverOpts, solver.OverrideHint(commitmentInfo[i].HintID, func(i int) solver.Hint {
			return func(_ *big.Int, in []*big.Int, out []*big.Int) error {
				privateCommittedValues[i] = make([]fr.Element, len(commitmentInfo[i].PrivateCommitted))
				hashed := in[:len(commitmentInfo[i].PublicAndCommitmentCommitted)]
				committed := in[len(hashed):]
				for j, inJ := range committed {
					privateCommittedValues[i][j].SetBigInt(inJ)
				}

				var err error
				if proof.Commitments[i], err = pk.CommitmentKeys[i].Commit(privateCommittedValues[i]); err != nil {
					return err
				}

				opt.HashToFieldFn.Write(constraint.SerializeCommitment(proof.Commitments[i].Marshal(), hashed, (fr.Bits-1)/8+1))
				hashBts := opt.HashToFieldFn.Sum(nil)
				opt.HashToFieldFn.Reset()
				nbBuf := fr.Bytes
				if opt.HashToFieldFn.Size() < fr.Bytes {
					nbBuf = opt.HashToFieldFn.Size()
				}
				var res fr.Element
				res.SetBytes(hashBts[:nbBuf])
				res.BigInt(out[0])
				return err
			}
		}(i)))
	}

	if r1cs.GkrInfo.Is() {
		var gkrData cs.GkrSolvingData
		solverOpts = append(solverOpts,
			solver.OverrideHint(r1cs.GkrInfo.SolveHintID, cs.GkrSolveHint(r1cs.GkrInfo, &gkrData)),
			solver.OverrideHint(r1cs.GkrInfo.ProveHintID, cs.GkrProveHint(r1cs.GkrInfo.HashName, &gkrData)))
	}

	_solution, err := r1cs.Solve(fullWitness, solverOpts...)
	if err != nil {
		return nil, err
	}

	solution := _solution.(*cs.R1CSSolution)
	wireValues := []fr.Element(solution.W)

	start := time.Now()

	commitmentsSerialized := make([]byte, fr.Bytes*len(commitmentInfo))
	for i := range commitmentInfo {
		copy(commitmentsSerialized[fr.Bytes*i:], wireValues[commitmentInfo[i].CommitmentIndex].Marshal())
	}

	if proof.CommitmentPok, err = pedersen.BatchProve(pk.CommitmentKeys, privateCommittedValues, commitmentsSerialized); err != nil {
		return nil, err
	}

	// H (witness reduction / FFT part)
	var h unsafe.Pointer
	chHDone := make(chan struct{}, 1)
	go func() {
		h = computeH(solution.A, solution.B, solution.C, pk)
		solution.A = nil
		solution.B = nil
		solution.C = nil
		chHDone <- struct{}{}
	}()

	// we need to copy and filter the wireValues for each multi exp
	// as pk.G1.A, pk.G1.B and pk.G2.B may have (a significant) number of point at infinity
	var wireValuesADevice, wireValuesBDevice iciclegnark.OnDeviceData
	chWireValuesA, chWireValuesB := make(chan struct{}, 1), make(chan struct{}, 1)

	go func() {
		wireValuesA := make([]fr.Element, len(wireValues)-int(pk.NbInfinityA))
		for i, j := 0, 0; j < len(wireValuesA); i++ {
			if pk.InfinityA[i] {
				continue
			}
			wireValuesA[j] = wireValues[i]
			j++
		}
		wireValuesASize := len(wireValuesA)
		scalarBytes := wireValuesASize * fr.Bytes

		// Copy scalars to the device and retain ptr to them
		copyDone := make(chan unsafe.Pointer, 1)
		iciclegnark.CopyToDevice(wireValuesA, scalarBytes, copyDone)
		wireValuesADevicePtr := <-copyDone

		wireValuesADevice = iciclegnark.OnDeviceData{
			P:    wireValuesADevicePtr,
			Size: wireValuesASize,
		}

		close(chWireValuesA)
	}()
	go func() {
		wireValuesB := make([]fr.Element, len(wireValues)-int(pk.NbInfinityB))
		for i, j := 0, 0; j < len(wireValuesB); i++ {
			if pk.InfinityB[i] {
				continue
			}
			wireValuesB[j] = wireValues[i]
			j++
		}
		wireValuesBSize := len(wireValuesB)
		scalarBytes := wireValuesBSize * fr.Bytes

		// Copy scalars to the device and retain ptr to them
		copyDone := make(chan unsafe.Pointer, 1)
		iciclegnark.CopyToDevice(wireValuesB, scalarBytes, copyDone)
		wireValuesBDevicePtr := <-copyDone

		wireValuesBDevice = iciclegnark.OnDeviceData{
			P:    wireValuesBDevicePtr,
			Size: wireValuesBSize,
		}

		close(chWireValuesB)
	}()

	// sample random r and s
	var r, s big.Int
	var _r, _s, _kr fr.Element
	if _, err := _r.SetRandom(); err != nil {
		return nil, err
	}
	if _, err := _s.SetRandom(); err != nil {
		return nil, err
	}
	_kr.Mul(&_r, &_s).Neg(&_kr)

	_r.BigInt(&r)
	_s.BigInt(&s)

	// computes r[δ], s[δ], kr[δ]
	deltas := curve.BatchScalarMultiplicationG1(&pk.G1.Delta, []fr.Element{_r, _s, _kr})

	var bs1, ar curve.G1Jac

	computeBS1 := func() error {
		<-chWireValuesB

		if bs1, _, err = iciclegnark.MsmOnDevice(wireValuesBDevice.P, pk.G1Device.B, wireValuesBDevice.Size, true); err != nil {
			return err
		}

		bs1.AddMixed(&pk.G1.Beta)
		bs1.AddMixed(&deltas[1])

		return nil
	}

	computeAR1 := func() error {
		<-chWireValuesA

		if ar, _, err = iciclegnark.MsmOnDevice(wireValuesADevice.P, pk.G1Device.A, wireValuesADevice.Size, true); err != nil {
			return err
		}

		ar.AddMixed(&pk.G1.Alpha)
		ar.AddMixed(&deltas[0])
		proof.Ar.FromJacobian(&ar)

		return nil
	}

	computeKRS := func() error {
		var krs, krs2, p1 curve.G1Jac
		sizeH := int(pk.Domain.Cardinality - 1) // comes from the fact the deg(H)=(n-1)+(n-1)-n=n-2

		// check for small circuits as iciclegnark doesn't handle zero sizes well
		if len(pk.G1.Z) > 0 {
			if krs2, _, err = iciclegnark.MsmOnDevice(h, pk.G1Device.Z, sizeH, true); err != nil {
				return err
			}
		}

		// filter the wire values if needed
		// TODO Perf @Tabaie worst memory allocation offender
		toRemove := commitmentInfo.GetPrivateCommitted()
		toRemove = append(toRemove, commitmentInfo.CommitmentIndexes())
		scalars := filterHeap(wireValues[r1cs.GetNbPublicVariables():], r1cs.GetNbPublicVariables(), internal.ConcatAll(toRemove...))

		// filter zero/infinity points since icicle doesn't handle them
		// See https://github.com/ingonyama-zk/icicle/issues/169 for more info
		for _, indexToRemove := range pk.InfinityPointIndicesK {
			scalars = append(scalars[:indexToRemove], scalars[indexToRemove+1:]...)
		}

		scalarBytes := len(scalars) * fr.Bytes

		copyDone := make(chan unsafe.Pointer, 1)
		iciclegnark.CopyToDevice(scalars, scalarBytes, copyDone)
		scalars_d := <-copyDone

		krs, _, err = iciclegnark.MsmOnDevice(scalars_d, pk.G1Device.K, len(scalars), true)
		iciclegnark.FreeDevicePointer(scalars_d)

		if err != nil {
			return err
		}

		krs.AddMixed(&deltas[2])

		krs.AddAssign(&krs2)

		p1.ScalarMultiplication(&ar, &s)
		krs.AddAssign(&p1)

		p1.ScalarMultiplication(&bs1, &r)
		krs.AddAssign(&p1)

		proof.Krs.FromJacobian(&krs)

		return nil
	}

	computeBS2 := func() error {
		// Bs2 (1 multi exp G2 - size = len(wires))
		var Bs, deltaS curve.G2Jac

		<-chWireValuesB
		if Bs, _, err = iciclegnark.MsmG2OnDevice(wireValuesBDevice.P, pk.G2Device.B, wireValuesBDevice.Size, true); err != nil {
			return err
		}

		deltaS.FromAffine(&pk.G2.Delta)
		deltaS.ScalarMultiplication(&deltaS, &s)
		Bs.AddAssign(&deltaS)
		Bs.AddMixed(&pk.G2.Beta)

		proof.Bs.FromJacobian(&Bs)
		return nil
	}

	// wait for FFT to end
	<-chHDone

	// schedule our proof part computations
	if err := computeAR1(); err != nil {
		return nil, err
	}
	if err := computeBS1(); err != nil {
		return nil, err
	}
	if err := computeKRS(); err != nil {
		return nil, err
	}
	if err := computeBS2(); err != nil {
		return nil, err
	}

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")

	// free device/GPU memory that is not needed for future proofs (scalars/hpoly)
	go func() {
		iciclegnark.FreeDevicePointer(wireValuesADevice.P)
		iciclegnark.FreeDevicePointer(wireValuesBDevice.P)
		iciclegnark.FreeDevicePointer(h)
	}()

	return proof, nil
}

// if len(toRemove) == 0, returns slice
// else, returns a new slice without the indexes in toRemove. The first value in the slice is taken as indexes as sliceFirstIndex
// this assumes len(slice) > len(toRemove)
// filterHeap modifies toRemove
func filterHeap(slice []fr.Element, sliceFirstIndex int, toRemove []int) (r []fr.Element) {

	if len(toRemove) == 0 {
		return slice
	}

	heap := utils.IntHeap(toRemove)
	heap.Heapify()

	r = make([]fr.Element, 0, len(slice))

	// note: we can optimize that for the likely case where len(slice) >>> len(toRemove)
	for i := 0; i < len(slice); i++ {
		if len(heap) > 0 && i+sliceFirstIndex == heap[0] {
			for len(heap) > 0 && i+sliceFirstIndex == heap[0] {
				heap.Pop()
			}
			continue
		}
		r = append(r, slice[i])
	}

	return
}

func computeH(a, b, c []fr.Element, pk *ProvingKey) unsafe.Pointer {
	// H part of Krs
	// Compute H (hz=ab-c, where z=-2 on ker X^n+1 (z(x)=x^n-1))
	// 	1 - _a = ifft(a), _b = ifft(b), _c = ifft(c)
	// 	2 - ca = fft_coset(_a), ba = fft_coset(_b), cc = fft_coset(_c)
	// 	3 - h = ifft_coset(ca o cb - cc)

	n := len(a)

	// add padding to ensure input length is domain cardinality
	padding := make([]fr.Element, int(pk.Domain.Cardinality)-n)
	a = append(a, padding...)
	b = append(b, padding...)
	c = append(c, padding...)
	n = len(a)

	sizeBytes := n * fr.Bytes

	/*********** Copy a,b,c to Device Start ************/
	// Individual channels are necessary to know which device pointers
	// point to which vector
	copyADone := make(chan unsafe.Pointer, 1)
	copyBDone := make(chan unsafe.Pointer, 1)
	copyCDone := make(chan unsafe.Pointer, 1)

	go iciclegnark.CopyToDevice(a, sizeBytes, copyADone)
	go iciclegnark.CopyToDevice(b, sizeBytes, copyBDone)
	go iciclegnark.CopyToDevice(c, sizeBytes, copyCDone)

	a_device := <-copyADone
	b_device := <-copyBDone
	c_device := <-copyCDone
	/*********** Copy a,b,c to Device End ************/

	computeInttNttDone := make(chan error, 1)
	computeInttNttOnDevice := func(devicePointer unsafe.Pointer) {
		a_intt_d := iciclegnark.INttOnDevice(devicePointer, pk.DomainDevice.TwiddlesInv, nil, n, sizeBytes, false)
		iciclegnark.NttOnDevice(devicePointer, a_intt_d, pk.DomainDevice.Twiddles, pk.DomainDevice.CosetTable, n, n, sizeBytes, true)
		computeInttNttDone <- nil
		iciclegnark.FreeDevicePointer(a_intt_d)
	}

	go computeInttNttOnDevice(a_device)
	go computeInttNttOnDevice(b_device)
	go computeInttNttOnDevice(c_device)
	_, _, _ = <-computeInttNttDone, <-computeInttNttDone, <-computeInttNttDone

	iciclegnark.PolyOps(a_device, b_device, c_device, pk.DenDevice, n)

	h := iciclegnark.INttOnDevice(a_device, pk.DomainDevice.TwiddlesInv, pk.DomainDevice.CosetTableInv, n, sizeBytes, true)

	go func() {
		iciclegnark.FreeDevicePointer(a_device)
		iciclegnark.FreeDevicePointer(b_device)
		iciclegnark.FreeDevicePointer(c_device)
	}()

	iciclegnark.ReverseScalars(h, n)

	return h
}
