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

package plonk

import (
	"math/big"

	bn254 "github.com/consensys/gnark-crypto/ecc/bn254/fr/polynomial"
	"github.com/consensys/gnark-crypto/polynomial"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/fft"

	bn254witness "github.com/consensys/gnark/internal/backend/bn254/witness"

	"github.com/consensys/gnark/internal/backend/bn254/cs"

	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
)

// ProofRaw PLONK proofs, consisting of opening proofs
type ProofRaw struct {

	// Claimed Values of L,R,O,Z,H at zeta (where H split in H1+X^m*H2+X^2m*H3, where deg(Hi)=m-1)
	LROZH [7]fr.Element

	// Claimed vales of Z(zX) at zeta
	ZShift fr.Element

	// Commitments to L,R,O,Z,H
	CommitmentsLROZH [7]polynomial.Digest

	// batch opening proofs for L,R,O,H,Z at zeta
	BatchOpenings polynomial.BatchOpeningProofSinglePoint

	// opening proof for Z at z*zeta
	OpeningZShift polynomial.OpeningProof
}

// ComputeLRO extracts the solution l, r, o, and returns it in lagrange form.
// solution = [ public | secret | internal ]
func ComputeLRO(spr *cs.SparseR1CS, publicData *PublicRaw, solution []fr.Element) (bn254.Polynomial, bn254.Polynomial, bn254.Polynomial, bn254.Polynomial) {

	s := int(publicData.DomainNum.Cardinality)

	var l, r, o, partialL bn254.Polynomial
	l = make([]fr.Element, s)
	r = make([]fr.Element, s)
	o = make([]fr.Element, s)
	partialL = make([]fr.Element, s)

	for i := 0; i < spr.NbPublicVariables; i++ { // placeholders
		l[i].Set(&solution[i])
		r[i].Set(&solution[0])
		o[i].Set(&solution[0])
	}
	offset := spr.NbPublicVariables
	for i := 0; i < len(spr.Constraints); i++ { // constraints
		l[offset+i].Set(&solution[spr.Constraints[i].L.VariableID()])
		r[offset+i].Set(&solution[spr.Constraints[i].R.VariableID()])
		o[offset+i].Set(&solution[spr.Constraints[i].O.VariableID()])
		partialL[offset+i].Set(&l[offset+i])
	}
	offset += len(spr.Constraints)
	for i := 0; i < len(spr.Assertions); i++ { // assertions
		l[offset+i].Set(&solution[spr.Assertions[i].L.VariableID()])
		r[offset+i].Set(&solution[spr.Assertions[i].R.VariableID()])
		o[offset+i].Set(&solution[spr.Assertions[i].O.VariableID()])
		partialL[offset+i].Set(&l[offset+i])
	}
	offset += len(spr.Assertions)
	for i := 0; i < s-offset; i++ { // offset to reach 2**n constraints (where the id of l,r,o is 0, so we assign solution[0])
		l[offset+i].Set(&solution[0])
		r[offset+i].Set(&solution[0])
		o[offset+i].Set(&solution[0])
		partialL[offset+i].Set(&l[offset+i])
	}

	return l, r, o, partialL

}

// ComputeZ computes Z (in Lagrange basis), where:
//
// * Z of degree n (domainNum.Cardinality)
// * Z(1)=1
// 								   (l_i+z**i+gamma)*(r_i+u*z**i+gamma)*(o_i+u**2z**i+gamma)
//	* for i>0: Z(u**i) = Pi_{k<i} -------------------------------------------------------
//								     (l_i+s1+gamma)*(r_i+s2+gamma)*(o_i+s3+gamma)
//
//	* l, r, o are the solution in Lagrange basis
func ComputeZ(l, r, o bn254.Polynomial, publicData *PublicRaw, gamma fr.Element) bn254.Polynomial {

	z := make(bn254.Polynomial, publicData.DomainNum.Cardinality)
	nbElmts := int(publicData.DomainNum.Cardinality)

	var f [3]fr.Element
	var g [3]fr.Element
	var u [3]fr.Element
	u[0].SetOne()
	u[1].Set(&publicData.Shifter[0])
	u[2].Set(&publicData.Shifter[1])

	z[0].SetOne()

	for i := 0; i < nbElmts-1; i++ {

		f[0].Add(&l[i], &u[0]).Add(&f[0], &gamma) //l_i+z**i+gamma
		f[1].Add(&r[i], &u[1]).Add(&f[1], &gamma) //r_i+u*z**i+gamma
		f[2].Add(&o[i], &u[2]).Add(&f[2], &gamma) //o_i+u**2*z**i+gamma

		g[0].Add(&l[i], &publicData.LS1[i]).Add(&g[0], &gamma) //l_i+z**i+gamma
		g[1].Add(&r[i], &publicData.LS2[i]).Add(&g[1], &gamma) //r_i+u*z**i+gamma
		g[2].Add(&o[i], &publicData.LS3[i]).Add(&g[2], &gamma) //o_i+u**2*z**i+gamma

		f[0].Mul(&f[0], &f[1]).Mul(&f[0], &f[2]) // (l_i+z**i+gamma)*(r_i+u*z**i+gamma)*(o_i+u**2z**i+gamma)
		g[0].Mul(&g[0], &g[1]).Mul(&g[0], &g[2]) //  (l_i+s1+gamma)*(r_i+s2+gamma)*(o_i+s3+gamma)

		z[i+1].Mul(&z[i], &f[0]).Div(&z[i+1], &g[0])

		u[0].Mul(&u[0], &publicData.DomainNum.Generator) // z**i -> z**i+1
		u[1].Mul(&u[1], &publicData.DomainNum.Generator) // u*z**i -> u*z**i+1
		u[2].Mul(&u[2], &publicData.DomainNum.Generator) // u**2*z**i -> u**2*z**i+1
	}

	return z

}

// evalConstraints computes the evaluation of lL+qrR+qqmL.R+qoO+k on
// the odd cosets of (Z/8mZ)/(Z/mZ), where m=nbConstraints+nbAssertions.
func evalConstraints(publicData *PublicRaw, evalL, evalR, evalO []fr.Element) []fr.Element {

	res := make([]fr.Element, 4*publicData.DomainNum.Cardinality)

	// evaluates ql, qr, qm, qo, k on the odd cosets of (Z/8mZ)/(Z/mZ)
	evalQl := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalQr := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalQm := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalQo := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalQk := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evaluateCosets(publicData.Ql, evalQl, publicData.DomainNum)
	evaluateCosets(publicData.Qr, evalQr, publicData.DomainNum)
	evaluateCosets(publicData.Qm, evalQm, publicData.DomainNum)
	evaluateCosets(publicData.Qo, evalQo, publicData.DomainNum)
	evaluateCosets(publicData.Qk, evalQk, publicData.DomainNum)

	// computes the evaluation of qrR+qlL+qmL.R+qoO+k on the odd cosets
	// of (Z/8mZ)/(Z/mZ)
	var acc, buf fr.Element
	for i := uint64(0); i < 4*publicData.DomainNum.Cardinality; i++ {

		acc.Mul(&evalQl[i], &evalL[i]) // ql.l

		buf.Mul(&evalQr[i], &evalR[i])
		acc.Add(&acc, &buf) // ql.l + qr.r

		buf.Mul(&evalQm[i], &evalL[i]).Mul(&buf, &evalR[i])
		acc.Add(&acc, &buf) // ql.l + qr.r + qm.l.r

		buf.Mul(&evalQo[i], &evalO[i])
		acc.Add(&acc, &buf)          // ql.l + qr.r + qm.l.r + qo.o
		res[i].Add(&acc, &evalQk[i]) // ql.l + qr.r + qm.l.r + qo.o + k
	}

	return res
}

// evalIDCosets id, uid, u**2id on the odd cosets of (Z/8mZ)/(Z/mZ)
func evalIDCosets(publicData *PublicRaw) (id, uid, uuid bn254.Polynomial) {

	// evaluation of id, uid, u**id on the cosets
	id = make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	c := int(publicData.DomainNum.Cardinality)
	id[0].SetOne()
	id[1].SetOne()
	id[2].SetOne()
	id[3].SetOne()
	for i := 1; i < c; i++ {
		id[4*i].Mul(&id[4*(i-1)], &publicData.DomainNum.Generator)
		id[4*i+1].Set(&id[4*i])
		id[4*i+2].Set(&id[4*i])
		id[4*i+3].Set(&id[4*i])
	}
	// at this stage, id = [1,1,1,1,|z,z,z,z|,...,|z**n-1,z**n-1,z**n-1,z**n-1]

	var uu fr.Element
	uu.Square(&publicData.DomainNum.FinerGenerator)
	var u [4]fr.Element
	u[0].Set(&publicData.DomainNum.FinerGenerator)                // u
	u[1].Mul(&u[0], &uu)                                          // u**3
	u[2].Mul(&u[1], &uu)                                          // u**5
	u[3].Mul(&u[2], &uu)                                          // u**7
	uid = make([]fr.Element, 4*publicData.DomainNum.Cardinality)  // shifter[0]*ID evaluated on odd cosets of (Z/8mZ)/(Z/mZ)
	uuid = make([]fr.Element, 4*publicData.DomainNum.Cardinality) // shifter[1]*ID evaluated on odd cosets of (Z/8mZ)/(Z/mZ)
	for i := 0; i < c; i++ {

		id[4*i].Mul(&id[4*i], &u[0])     // coset u.<1,z,..,z**n-1>
		id[4*i+1].Mul(&id[4*i+1], &u[1]) // coset u**3.<1,z,..,z**n-1>
		id[4*i+2].Mul(&id[4*i+2], &u[2]) // coset u**5.<1,z,..,z**n-1>
		id[4*i+3].Mul(&id[4*i+3], &u[3]) // coset u**7.<1,z,..,z**n-1>

		uid[4*i].Mul(&id[4*i], &publicData.Shifter[0])     // shifter[0]*ID
		uid[4*i+1].Mul(&id[4*i+1], &publicData.Shifter[0]) // shifter[0]*ID
		uid[4*i+2].Mul(&id[4*i+2], &publicData.Shifter[0]) // shifter[0]*ID
		uid[4*i+3].Mul(&id[4*i+3], &publicData.Shifter[0]) // shifter[0]*ID

		uuid[4*i].Mul(&id[4*i], &publicData.Shifter[1])     // shifter[1]*ID
		uuid[4*i+1].Mul(&id[4*i+1], &publicData.Shifter[1]) // shifter[1]*ID
		uuid[4*i+2].Mul(&id[4*i+2], &publicData.Shifter[1]) // shifter[1]*ID
		uuid[4*i+3].Mul(&id[4*i+3], &publicData.Shifter[1]) // shifter[1]*ID

	}
	return
}

// evalConstraintOrdering computes the evaluation of Z(uX)g1g2g3-Z(X)f1f2f3 on the odd
// cosets of (Z/8mZ)/(Z/mZ), where m=nbConstraints+nbAssertions.
//
// z: permutation accumulator polynomial in canonical form
// l, r, o: solution, in canonical form
func evalConstraintOrdering(publicData *PublicRaw, evalZ, evalZu, evalL, evalR, evalO bn254.Polynomial, gamma fr.Element) bn254.Polynomial {

	// evaluation of z, zu, s1, s2, s3, on the odd cosets of (Z/8mZ)/(Z/mZ)
	evalS1 := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalS2 := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalS3 := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evaluateCosets(publicData.CS1, evalS1, publicData.DomainNum)
	evaluateCosets(publicData.CS2, evalS2, publicData.DomainNum)
	evaluateCosets(publicData.CS3, evalS3, publicData.DomainNum)

	// evalutation of ID, u*ID, u**2*ID on the odd cosets of (Z/8mZ)/(Z/mZ)
	evalID, evaluID, evaluuID := evalIDCosets(publicData)

	// computes Z(uX)g1g2g3l-Z(X)f1f2f3l on the odd cosets of (Z/8mZ)/(Z/mZ)
	res := make(bn254.Polynomial, 4*publicData.DomainNum.Cardinality)

	var f [3]fr.Element
	var g [3]fr.Element
	for i := 0; i < 4*int(publicData.DomainNum.Cardinality); i++ {

		f[0].Add(&evalL[i], &evalID[i]).Add(&f[0], &gamma)   //l_i+z**i+gamma
		f[1].Add(&evalR[i], &evaluID[i]).Add(&f[1], &gamma)  //r_i+u*z**i+gamma
		f[2].Add(&evalO[i], &evaluuID[i]).Add(&f[2], &gamma) //o_i+u**2*z**i+gamma

		g[0].Add(&evalL[i], &evalS1[i]).Add(&g[0], &gamma) //l_i+s1+gamma
		g[1].Add(&evalR[i], &evalS2[i]).Add(&g[1], &gamma) //r_i+s2+gamma
		g[2].Add(&evalO[i], &evalS3[i]).Add(&g[2], &gamma) //o_i+s3+gamma

		f[0].Mul(&f[0], &f[1]).
			Mul(&f[0], &f[2]).
			Mul(&f[0], &evalZ[i]) // z_i*(l_i+z**i+gamma)*(r_i+u*z**i+gamma)*(o_i+u**2*z**i+gamma)

		g[0].Mul(&g[0], &g[1]).
			Mul(&g[0], &g[2]).
			Mul(&g[0], &evalZu[i]) // u*z_i*(l_i+s1+gamma)*(r_i+s2+gamma)*(o_i+s3+gamma)

		res[i].Sub(&g[0], &f[0])
	}

	return res
}

// evalStartsAtOne computes the evaluation of L1*(z-1) on the odd cosets
// of (Z/8mZ)/(Z/mZ).
//
// evalZ is the evaluation of z (=permutation constraint polynomial) on odd cosets of (Z/8mZ)/(Z/mZ)
func evalStartsAtOne(publicData *PublicRaw, evalZ bn254.Polynomial) bn254.Polynomial {

	// computes L1 (canonical form)
	lOneLagrange := make([]fr.Element, publicData.DomainNum.Cardinality)
	lOneLagrange[0].SetOne()
	publicData.DomainNum.FFTInverse(lOneLagrange, fft.DIF, 0)
	fft.BitReverse(lOneLagrange)

	// evaluates L1 on the odd cosets of (Z/8mZ)/(Z/mZ)
	res := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evaluateCosets(lOneLagrange, res, publicData.DomainNum)

	// // evaluates L1*(z-1) on the odd cosets of (Z/8mZ)/(Z/mZ)
	var buf, one fr.Element
	one.SetOne()
	for i := 0; i < 4*int(publicData.DomainNum.Cardinality); i++ {
		buf.Sub(&evalZ[i], &one)
		res[i].Mul(&buf, &res[i])
	}

	return res
}

// evaluateCosets evaluates poly (canonical form) of degree m=domainNum.Cardinality on
// the 4 odd cosets of (Z/8mZ)/(Z/mZ), so it dodges Z/mZ (+Z/2kmZ), which contains the
// vanishing set of Z.
//
// Puts the result in res (of size 4*domain.Cardinality).
//
// Both sizes of poly and res are powers of 2, len(res) = 4*len(poly).
func evaluateCosets(poly, res []fr.Element, domain *fft.Domain) {

	// build a copy of poly padded with 0 so it has the length of the closest power of 2 of poly
	evaluations := make([][]fr.Element, 4)
	evaluations[0] = make([]fr.Element, domain.Cardinality)
	evaluations[1] = make([]fr.Element, domain.Cardinality)
	evaluations[2] = make([]fr.Element, domain.Cardinality)
	evaluations[3] = make([]fr.Element, domain.Cardinality)

	// evaluations[i] must contain poly in the canonical basis
	copy(evaluations[0], poly)
	copy(evaluations[1], poly)
	copy(evaluations[2], poly)
	copy(evaluations[3], poly)

	domain.FFT(evaluations[0], fft.DIF, 1)
	domain.FFT(evaluations[1], fft.DIF, 3)
	domain.FFT(evaluations[2], fft.DIF, 5)
	domain.FFT(evaluations[3], fft.DIF, 7)
	fft.BitReverse(evaluations[0])
	fft.BitReverse(evaluations[1])
	fft.BitReverse(evaluations[2])
	fft.BitReverse(evaluations[3])

	for i := uint64(0); i < domain.Cardinality; i++ {
		res[4*i].Set(&evaluations[0][i])
		res[4*i+1].Set(&evaluations[1][i])
		res[4*i+2].Set(&evaluations[2][i])
		res[4*i+3].Set(&evaluations[3][i])
	}
}

// shiftZ turns z to z(uX) (both in Lagrange basis)
func shiftZ(z bn254.Polynomial) bn254.Polynomial {

	res := make(bn254.Polynomial, len(z))
	copy(res, z)

	var buf fr.Element
	buf.Set(&res[0])
	for i := 0; i < len(res)-1; i++ {
		res[i].Set(&res[i+1])
	}
	res[len(res)-1].Set(&buf)

	return res
}

// computeH computes h in canonical form, split as h1+X^mh2+X^2mh3 such that
//
// qlL+qrR+qmL.R+qoO+k + alpha.(zu*g1*g2*g3*l-z*f1*f2*f3*l) + alpha**2*L1*(z-1)= h.Z
// \------------------/         \------------------------/             \-----/
//    constraintsInd			    constraintOrdering					startsAtOne
//
// constraintInd, constraintOrdering are evaluated on the odd cosets of (Z/8mZ)/(Z/mZ)
func computeH(publicData *PublicRaw, constraintsInd, constraintOrdering, startsAtOne bn254.Polynomial, alpha fr.Element) (bn254.Polynomial, bn254.Polynomial, bn254.Polynomial) {

	h := make(bn254.Polynomial, publicData.DomainH.Cardinality)

	// evaluate Z = X**m-1 on the odd cosets of (Z/8mZ)/(Z/mZ)
	var bExpo big.Int
	bExpo.SetUint64(publicData.DomainNum.Cardinality)
	var u [4]fr.Element
	var uu fr.Element
	var one fr.Element
	one.SetOne()
	uu.Square(&publicData.DomainNum.FinerGenerator)
	u[0].Set(&publicData.DomainNum.FinerGenerator)
	u[1].Mul(&u[0], &uu)
	u[2].Mul(&u[1], &uu)
	u[3].Mul(&u[2], &uu)
	u[0].Exp(u[0], &bExpo).Sub(&u[0], &one).Inverse(&u[0]) // (X**m-1)**-1 at u
	u[1].Exp(u[1], &bExpo).Sub(&u[1], &one).Inverse(&u[1]) // (X**m-1)**-1 at u**3
	u[2].Exp(u[2], &bExpo).Sub(&u[2], &one).Inverse(&u[2]) // (X**m-1)**-1 at u**5
	u[3].Exp(u[3], &bExpo).Sub(&u[3], &one).Inverse(&u[3]) // (X**m-1)**-1 at u**7

	// evaluate qlL+qrR+qmL.R+qoO+k + alpha.(zu*g1*g2*g3*l-z*f1*f2*f3*l) + alpha**2*L1(X)(Z(X)-1)
	// on the odd cosets of (Z/8mZ)/(Z/mZ)
	for i := 0; i < 4*int(publicData.DomainNum.Cardinality); i++ {
		h[i].Mul(&startsAtOne[i], &alpha).
			Add(&h[i], &constraintOrdering[i]).
			Mul(&h[i], &alpha).
			Add(&h[i], &constraintsInd[i])
	}

	// evaluate qlL+qrR+qmL.R+qoO+k + alpha.(zu*g1*g2*g3*l-z*f1*f2*f3*l)/Z
	// on the odd cosets of (Z/8mZ)/(Z/mZ)
	for i := 0; i < int(publicData.DomainNum.Cardinality); i++ {
		h[4*i].Mul(&h[4*i], &u[0])
		h[4*i+1].Mul(&h[4*i+1], &u[1])
		h[4*i+2].Mul(&h[4*i+2], &u[2])
		h[4*i+3].Mul(&h[4*i+3], &u[3])
	}

	// put h in canonical form
	publicData.DomainH.FFTInverse(h, fft.DIF, 1)
	fft.BitReverse(h)

	h1 := make(bn254.Polynomial, publicData.DomainNum.Cardinality)
	h2 := make(bn254.Polynomial, publicData.DomainNum.Cardinality)
	h3 := make(bn254.Polynomial, publicData.DomainNum.Cardinality)
	copy(h1, h[:publicData.DomainNum.Cardinality])
	copy(h2, h[publicData.DomainNum.Cardinality:2*publicData.DomainNum.Cardinality])
	copy(h3, h[2*publicData.DomainNum.Cardinality:3*publicData.DomainNum.Cardinality])

	return h1, h2, h3

}

// ProveRaw from the public data
// TODO add a parameter to force the resolution of the system even if a constraint does not hold
func ProveRaw(spr *cs.SparseR1CS, publicData *PublicRaw, fullWitness bn254witness.Witness) (*ProofRaw, error) {

	// create a transcript manager to apply Fiat Shamir
	fs := fiatshamir.NewTranscript(fiatshamir.SHA256, "gamma", "alpha", "zeta")

	// result
	proof := &ProofRaw{}

	// compute the solution
	solution, _ := spr.Solve(fullWitness)

	// query l, r, o in Lagrange basis
	ll, lr, lo, partialL := ComputeLRO(spr, publicData, solution)

	// save ll, lr, lo, and make a copy of them in canonical basis.
	// We commit them and derive gamma from them.
	cl := make(bn254.Polynomial, len(ll))
	cr := make(bn254.Polynomial, len(lr))
	co := make(bn254.Polynomial, len(lo))
	copy(cl, ll)
	copy(cr, lr)
	copy(co, lo)
	publicData.DomainNum.FFTInverse(cl, fft.DIF, 0)
	publicData.DomainNum.FFTInverse(cr, fft.DIF, 0)
	publicData.DomainNum.FFTInverse(co, fft.DIF, 0)
	publicData.DomainNum.FFTInverse(partialL, fft.DIF, 0)
	fft.BitReverse(cl)
	fft.BitReverse(cr)
	fft.BitReverse(co)
	fft.BitReverse(partialL)

	// derive gamma from the Comm(l), Comm(r), Comm(o)
	var err error
	proof.CommitmentsLROZH[0], err = publicData.CommitmentScheme.Commit(&cl)
	if err != nil {
		return proof, err
	}
	proof.CommitmentsLROZH[1], err = publicData.CommitmentScheme.Commit(&cr)
	if err != nil {
		return proof, err
	}
	proof.CommitmentsLROZH[2], err = publicData.CommitmentScheme.Commit(&co)
	if err != nil {
		return proof, err
	}
	err = fs.Bind("gamma", proof.CommitmentsLROZH[0].Marshal())
	if err != nil {
		return proof, err
	}
	err = fs.Bind("gamma", proof.CommitmentsLROZH[1].Marshal())
	if err != nil {
		return proof, err
	}
	err = fs.Bind("gamma", proof.CommitmentsLROZH[2].Marshal())
	if err != nil {
		return proof, err
	}
	bgamma, err := fs.ComputeChallenge("gamma")
	if err != nil {
		return proof, err
	}
	var gamma fr.Element
	gamma.SetBytes(bgamma)

	// compute Z, the permutation accumulator polynomial, in Lagrange basis
	z := ComputeZ(ll, lr, lo, publicData, gamma)

	// compute Z(uX), in Lagrange basis
	zu := shiftZ(z)

	// compute the evaluations of l, r, o on odd cosets of (Z/8mZ)/(Z/mZ)
	evalL := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalR := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalO := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evaluateCosets(cl, evalL, publicData.DomainNum)
	evaluateCosets(cr, evalR, publicData.DomainNum)
	evaluateCosets(co, evalO, publicData.DomainNum)

	// compute the evaluation of qlL+qrR+qmL.R+qoO+k on the odd cosets of (Z/8mZ)/(Z/mZ)
	constraintsInd := evalConstraints(publicData, evalL, evalR, evalO)

	// put back z, zu in canonical basis
	publicData.DomainNum.FFTInverse(z, fft.DIF, 0)
	publicData.DomainNum.FFTInverse(zu, fft.DIF, 0)
	fft.BitReverse(z)
	fft.BitReverse(zu)

	// evaluate z, zu on the odd cosets of (Z/8mZ)/(Z/mZ)
	evalZ := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evalZu := make([]fr.Element, 4*publicData.DomainNum.Cardinality)
	evaluateCosets(z, evalZ, publicData.DomainNum)
	evaluateCosets(zu, evalZu, publicData.DomainNum)

	// compute zu*g1*g2*g3-z*f1*f2*f3 on the odd cosets of (Z/8mZ)/(Z/mZ)
	constraintsOrdering := evalConstraintOrdering(publicData, evalZ, evalZu, evalL, evalR, evalO, gamma)

	// compute L1*(z-1) on the odd cosets of (Z/8mZ)/(Z/mZ)
	startsAtOne := evalStartsAtOne(publicData, evalZ)

	// commit to Z
	proof.CommitmentsLROZH[3], err = publicData.CommitmentScheme.Commit(&z)
	if err != nil {
		return proof, err
	}

	// derive alpha from the Comm(l), Comm(r), Comm(o), Com(Z)
	err = fs.Bind("alpha", proof.CommitmentsLROZH[3].Marshal())
	if err != nil {
		return proof, err
	}
	balpha, err := fs.ComputeChallenge("alpha")
	if err != nil {
		return proof, err
	}
	var alpha fr.Element
	alpha.SetBytes(balpha)

	// compute h in canonical form
	h1, h2, h3 := computeH(publicData, constraintsInd, constraintsOrdering, startsAtOne, alpha)

	// commit to h (3 commitments h1 + x**n*h2 + x**2n*h3)
	proof.CommitmentsLROZH[4], err = publicData.CommitmentScheme.Commit(&h1)
	if err != nil {
		return proof, err
	}
	proof.CommitmentsLROZH[5], err = publicData.CommitmentScheme.Commit(&h2)
	if err != nil {
		return proof, err
	}
	proof.CommitmentsLROZH[6], err = publicData.CommitmentScheme.Commit(&h3)
	if err != nil {
		return proof, err
	}

	// derive zeta, the point of evaluation
	err = fs.Bind("zeta", proof.CommitmentsLROZH[4].Marshal())
	if err != nil {
		return proof, err
	}
	err = fs.Bind("zeta", proof.CommitmentsLROZH[5].Marshal())
	if err != nil {
		return proof, err
	}
	err = fs.Bind("zeta", proof.CommitmentsLROZH[6].Marshal())
	if err != nil {
		return proof, err
	}
	bzeta, err := fs.ComputeChallenge("zeta")
	if err != nil {
		return proof, err
	}
	var zeta fr.Element
	zeta.SetBytes(bzeta)

	// compute evaluations of l, r, o, z at zeta
	proof.LROZH[0].SetInterface(partialL.Eval(&zeta).(fr.Element))
	proof.LROZH[1].SetInterface(cr.Eval(&zeta).(fr.Element))
	proof.LROZH[2].SetInterface(co.Eval(&zeta).(fr.Element))
	proof.LROZH[3].SetInterface(z.Eval(&zeta).(fr.Element))

	// compute evaluations of h1, h2, h3 at zeta (so h(zeta)=h1(zeta)+zeta^m*h2(zeta)+zeta^2m*h3(zeta))
	proof.LROZH[4].SetInterface(h1.Eval(&zeta))
	proof.LROZH[5].SetInterface(h2.Eval(&zeta))
	proof.LROZH[6].SetInterface(h3.Eval(&zeta))

	// compute evaluation of z at z*zeta
	var zzeta fr.Element
	zzeta.Mul(&zeta, &publicData.DomainNum.Generator)
	proof.ZShift.SetInterface(z.Eval(&zzeta))

	// compute batfch opening proof for l, r, o, h, z at zeta
	polynomialsToOpenAtZeta := []polynomial.Polynomial{&cl, &cr, &co, &z, &h1, &h2, &h3}
	proof.BatchOpenings, err = publicData.CommitmentScheme.BatchOpenSinglePoint(&zeta, proof.CommitmentsLROZH[:], polynomialsToOpenAtZeta)
	if err != nil {
		return proof, err
	}

	// compute opening proof for z at z*zeta
	proof.OpeningZShift, err = publicData.CommitmentScheme.Open(&zzeta, &z)
	if err != nil {
		return proof, err
	}

	return proof, nil
}
